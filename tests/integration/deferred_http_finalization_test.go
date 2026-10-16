//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

// Gate the actual original delivery after native acceptance and resource
// publication. Concurrent financial observers can then win the resource lock
// before finish supplies the original HTTP outcome.
type deferredDeliveryGate struct {
	header           http.Header
	status           int
	body             bytes.Buffer
	offered          []byte
	entered, release chan struct{}
	once             sync.Once
	fail             bool
	cancelRequest    context.CancelFunc
}

func (w *deferredDeliveryGate) Header() http.Header              { return w.header }
func (w *deferredDeliveryGate) WriteHeader(status int)           { w.status = status }
func (w *deferredDeliveryGate) SetReadDeadline(time.Time) error  { return nil }
func (w *deferredDeliveryGate) SetWriteDeadline(time.Time) error { return nil }
func (w *deferredDeliveryGate) Flush()                           {}
func (w *deferredDeliveryGate) Write(body []byte) (int, error) {
	w.once.Do(func() {
		w.offered = append([]byte(nil), body...)
		close(w.entered)
	})
	<-w.release
	if w.fail {
		return 0, io.ErrClosedPipe
	}
	return w.body.Write(body)
}

func pausedDeferredResponse(t *testing.T, fail bool) (*accessHarness, *resources.Resource, *usage.Event, *deferredDeliveryGate, <-chan struct{}, *captureSink) {
	t.Helper()
	fixture, endpoint := newResponseObservationFixture(t)
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAI(t, h, endpoint,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	fixture.resps["resp-up-1"]["status"] = "in_progress"
	fixture.resps["resp-up-1"]["usage"] = nil
	sink := &captureSink{}
	h.Gateway.Sink = sink
	writer := &deferredDeliveryGate{header: http.Header{}, entered: make(chan struct{}), release: make(chan struct{}), fail: fail}
	requestContext, cancelRequest := context.WithCancel(t.Context())
	writer.cancelRequest = cancelRequest
	t.Cleanup(cancelRequest)
	request := httptest.NewRequestWithContext(requestContext, http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"`+slug+`","input":"original outcome","store":true,"background":true}`))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HTTP.Config.Handler.ServeHTTP(writer, request)
	}()
	t.Cleanup(func() {
		select {
		case <-writer.release:
		default:
			close(writer.release)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("original delivery did not finish after release")
		}
	})
	select {
	case <-writer.entered:
	case <-done:
		t.Fatalf("original exchange finished before delivery gate: %d %s", writer.status, writer.body.String())
	case <-time.After(15 * time.Second):
		t.Fatal("original delivery did not reach gate")
	}
	var local uuid.UUID
	var keyID string
	var raw []byte
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text,api_key_id::text,metadata->'pending_usage' FROM olp.provider_resources WHERE route_slug=$1 AND upstream_id='resp-up-1'`, slug).Scan(&local, &keyID, &raw); err != nil {
		t.Fatalf("retained pending owner unavailable before original HTTP delivery (status %d, offered %s): %v", writer.status, writer.offered, err)
	}
	resource, err := h.Gateway.Resources.Get(t.Context(), resources.KindResponse, keyID, resources.LocalID(resources.KindResponse, local))
	if err != nil {
		t.Fatal(err)
	}
	event, err := usage.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !event.HTTPOutcomePending || event.StatusCode != nil || event.Committed || event.Attempts[len(event.Attempts)-1].Committed {
		t.Fatalf("unfinalized original exchange invented HTTP success: %+v", event)
	}
	var rows int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.requests WHERE id=$1`, event.RequestID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("pending generation inserted history before financial settlement: %d %v", rows, err)
	}
	return h, resource, event, writer, done, sink
}

func deferredCompletion(event *usage.Event, env gateway.Envelope) usage.HTTPCompletion {
	final := event.Attempts[len(event.Attempts)-1]
	fact := env.Attempts[len(env.Attempts)-1]
	status := func(value int) *int {
		if value == 0 {
			return nil
		}
		return &value
	}
	text := func(value string) *string {
		if value == "" || value == "success" {
			return nil
		}
		return &value
	}
	timing := func(value *time.Duration) *int64 {
		if value == nil {
			return nil
		}
		ms := max(value.Milliseconds(), 0)
		return &ms
	}
	client := ""
	if fact.Interaction != nil {
		client = fact.Interaction.ClientState
	}
	return usage.HTTPCompletion{EventID: event.EventID, RequestID: event.RequestID, APIKeyID: event.APIKeyID,
		RequestStartedAt: event.RequestStartedAt, CompletedAt: env.CompletedAt, StatusCode: status(env.Status), ErrorClass: text(env.ErrorClass),
		LatencyMS: max(env.Duration.Milliseconds(), 0), FirstByteMS: timing(env.FirstByte), BudgetBoundary: env.BudgetBoundary, PayloadCaptured: env.PayloadCaptured,
		Attempt: usage.HTTPAttemptCompletion{ID: final.ID, Ordinal: final.Ordinal, ProviderID: final.ProviderID, UpstreamModel: final.UpstreamModel,
			StartedAt: final.StartedAt, CompletedAt: fact.StartedAt.Add(max(fact.Duration, 0)), StatusCode: status(fact.Status), ErrorClass: text(fact.Class),
			Committed: fact.Committed, LatencyMS: max(fact.Duration.Milliseconds(), 0), FirstByteMS: timing(fact.FirstByte), ClientState: client}}
}

func assertDeferredHTTPUnknown(t *testing.T, h *accessHarness, event *usage.Event) {
	t.Helper()
	var unknown bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT r.completed_at IS NULL AND r.status_code IS NULL AND r.error_class IS NULL
	 AND r.total_latency_ms IS NULL AND r.first_byte_ms IS NULL AND a.completed_at IS NULL AND a.latency_ms IS NULL AND NOT a.committed
	 FROM olp.requests r JOIN olp.attempts a ON a.request_id=r.id AND a.request_started_at=r.started_at WHERE r.id=$1 AND a.id=$2`,
		event.RequestID, event.Attempts[len(event.Attempts)-1].ID).Scan(&unknown); err != nil || !unknown {
		t.Fatalf("unfinalized exchange is not explicitly unknown: %v %v", unknown, err)
	}
}

func assertDeferredHTTPKnown(t *testing.T, h *accessHarness, event *usage.Event, c usage.HTTPCompletion) {
	t.Helper()
	var matches bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT r.completed_at=$3 AND r.status_code IS NOT DISTINCT FROM $4::integer
	 AND r.error_class IS NOT DISTINCT FROM $5::text AND r.total_latency_ms=$6 AND r.first_byte_ms IS NOT DISTINCT FROM $7::integer
	 AND a.completed_at=$8 AND a.status_code IS NOT DISTINCT FROM $9::integer AND a.error_class IS NOT DISTINCT FROM $10::text
	 AND a.committed=$11 AND a.latency_ms=$12 AND a.first_byte_ms IS NOT DISTINCT FROM $13::integer
	 FROM olp.requests r JOIN olp.attempts a ON a.request_id=r.id AND a.request_started_at=r.started_at WHERE r.id=$1 AND a.id=$2`,
		event.RequestID, c.Attempt.ID, c.CompletedAt, c.StatusCode, c.ErrorClass, c.LatencyMS, c.FirstByteMS,
		c.Attempt.CompletedAt, c.Attempt.StatusCode, c.Attempt.ErrorClass, c.Attempt.Committed, c.Attempt.LatencyMS, c.Attempt.FirstByteMS).Scan(&matches); err != nil || !matches {
		t.Fatalf("original final outcome was not preserved: %v %v %+v", matches, err, c)
	}
}

func deferredFinancialSnapshot(t *testing.T, h *accessHarness, event *usage.Event) string {
	t.Helper()
	var snapshot string
	if err := h.Pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
	 'facts',(SELECT jsonb_agg(to_jsonb(f) ORDER BY attempt_ordinal) FROM olp.attempt_usage_facts f WHERE request_id=$1),
	 'receipts',(SELECT jsonb_agg(to_jsonb(r)) FROM olp.request_metadata_event_receipts r WHERE event_id=$2 AND request_id=$1),
	 'costs',(SELECT jsonb_agg(to_jsonb(c) ORDER BY window_kind,window_id) FROM olp.api_key_cost_windows c WHERE api_key_id=$3))::text`,
		event.RequestID, event.EventID, event.APIKeyID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
