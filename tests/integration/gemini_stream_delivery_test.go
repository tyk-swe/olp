//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/usage"
)

// Allow earlier native frames through when the failure is gated at completion.
// Observation and settlement must have run before the chosen client write.
type geminiStreamDeliveryGate struct {
	*deferredDeliveryGate
	terminal bool
	flush    bool
	pending  []byte
}

type geminiStreamReadGate struct {
	partial   bool
	malformed bool
	entered   chan struct{}
}

type geminiWorkingStreamWriter struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
	once    sync.Once
	cancel  context.CancelFunc
}

func (w *geminiWorkingStreamWriter) Flush() {
	w.ResponseRecorder.Flush()
	if w.cancel != nil {
		w.cancel()
	}
	w.once.Do(func() { close(w.flushed) })
}

func TestGeminiInteractionStreamReadCancellation(t *testing.T) {
	for _, operation := range []string{"generation", "retrieval"} {
		for _, interruption := range []string{"before-frame", "after-partial", "malformed-after-partial", "route-timeout"} {
			t.Run(operation+"/"+interruption, func(t *testing.T) {
				h := newAccessHarness(t)
				native := newGeminiLifecycleProvider(t)
				native.partialUsage.Store(true)
				var active atomic.Pointer[geminiStreamReadGate]
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gate := active.Load()
					if gate == nil || !strings.HasPrefix(r.URL.Path, "/v1beta/interactions") {
						native.Config.Handler.ServeHTTP(w, r)
						return
					}
					reply := httptest.NewRecorder()
					native.Config.Handler.ServeHTTP(reply, r)
					for name, values := range reply.Header() {
						w.Header()[name] = values
					}
					w.WriteHeader(reply.Code)
					if gate.partial {
						first, _, found := bytes.Cut(reply.Body.Bytes(), []byte("\n\n"))
						if !found {
							t.Error("native fixture omitted first SSE event")
							return
						}
						first = bytes.Replace(first, []byte(`"status":"in_progress"`), []byte(`"status":"in_progress","usage":`+geminiUsageCheckpoint(3, 2, 0)), 1)
						_, _ = w.Write(append(first, '\n', '\n'))
					}
					if gate.malformed {
						_, _ = io.WriteString(w, "data: {\"event_type\":\"step.start\",\"step\":\n\n")
					}
					w.(http.Flusher).Flush()
					close(gate.entered)
					if gate.malformed {
						return
					}
					<-r.Context().Done()
				}))
				t.Cleanup(proxy.Close)
				ownerBrowser := h.owner()
				slug, key, providerID := provisionGeminiLifecycle(t, h, ownerBrowser, "gemini-interactions", &geminiLifecycleProvider{Server: proxy})
				if interruption == "route-timeout" {
					slug = "gemini-read-timeout-" + uuid.NewString()
					draftInput := strictDraft(fidelityDraft(slug, providerID))
					draftInput["overall_timeout_ms"] = 1000
					draftInput["targets"].([]any)[0].(map[string]any)["timeout_ms"] = 1000
					draft := h.want(ownerBrowser, http.MethodPost, "/api/v1/route-drafts", draftInput, idem(uuid.NewString()), http.StatusCreated)
					h.want(ownerBrowser, http.MethodPost, "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), http.StatusOK)
					key = h.want(ownerBrowser, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "live parent timeout control", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "allow_provider_state": true}, idem(uuid.NewString()), http.StatusCreated)["secret"].(string)
					h.refresh()
				}
				method, path := http.MethodPost, "/gemini/v1beta/interactions"
				body := `{"model":"` + slug + `","input":"read cancellation","store":true,"stream":true}`
				var local, original, owner string
				if operation == "retrieval" {
					local, original, owner = pendingGeminiGeneration(t, h, slug, key)
					method, path, body = http.MethodGet, path+"/"+local+"?stream=true", ""
				}
				baseline := len(native.captured())
				gate := &geminiStreamReadGate{partial: strings.Contains(interruption, "after-partial"), malformed: interruption == "malformed-after-partial", entered: make(chan struct{})}
				active.Store(gate)
				sink := &captureSink{}
				h.Gateway.Sink = sink
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				writer := &geminiWorkingStreamWriter{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
				if gate.malformed {
					writer.cancel = cancel
				}
				request := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
				request.Header.Set("X-Goog-Api-Key", key)
				request.Header.Set("Content-Type", "application/json")
				done := make(chan struct{})
				t.Cleanup(func() {
					cancel()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("Gemini read did not finish after cancellation")
					}
				})
				go func() {
					defer close(done)
					h.HTTP.Config.Handler.ServeHTTP(writer, request)
				}()
				select {
				case <-gate.entered:
				case <-done:
					t.Fatalf("stream failed before native read gate: %s", writer.Body)
				case <-time.After(10 * time.Second):
					t.Fatal("provider did not enter gated stream")
				}
				if gate.partial {
					select {
					case <-writer.flushed:
					case <-done:
						t.Fatalf("partial event failed before successful delivery: %s", writer.Body)
					case <-time.After(5 * time.Second):
						t.Fatal("partial event was not delivered")
					}
				} else if interruption == "before-frame" {
					// The first native frame is held. Let the local HTTP reader
					// receive the flushed headers before canceling its lifetime.
					select {
					case <-time.After(50 * time.Millisecond):
					case <-done:
						t.Fatal("stream ended while the provider held its first frame")
					}
				}
				if interruption != "route-timeout" {
					cancel()
				}
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("interrupted SSE read did not finish")
				}
				event := sink.last()
				wantOutcome, wantCode, wantStatus, wantClass := "cancelled", "client_cancelled", 0, "cancelled"
				if gate.malformed {
					wantClass = "protocol"
				}
				if interruption == "route-timeout" {
					wantOutcome, wantCode, wantStatus, wantClass = "failure", "interaction_incomplete", http.StatusBadGateway, "protocol"
					if ctx.Err() != nil {
						t.Fatal("route timeout canceled the original caller")
					}
				}
				if sink.count() != 1 || event.Outcome != wantOutcome || event.ErrorClass != wantCode || event.Status != wantStatus || len(event.Attempts) != 1 {
					t.Fatalf("read interruption misclassified: %+v", event)
				}
				fact := event.Attempts[0]
				wantClient := usage.ClientUnobserved
				if gate.partial {
					wantClient = usage.ClientPartial
				}
				if fact.Class != wantClass || fact.Interaction == nil || fact.Interaction.UpstreamState != usage.UpstreamAccepted || fact.Interaction.ClientState != wantClient || event.Committed != gate.partial || (event.FirstByte != nil) != gate.partial {
					t.Fatalf("read interruption changed accepted/client evidence: %+v", event)
				}
				if operation == "generation" && !gate.partial && (fact.Usage != nil || fact.UsageObserved || fact.UsageComplete || !fact.BillingUncertain) {
					t.Fatalf("unobserved accepted work became certain: %+v", fact)
				}
				if operation == "generation" && gate.partial {
					original, owner = event.AccountingID, event.KeyID
					var delivered struct {
						Interaction struct{ ID string } `json:"interaction"`
					}
					_, data, found := bytes.Cut(writer.Body.Bytes(), []byte("data: "))
					if !found || json.Unmarshal(data, &delivered) != nil || delivered.Interaction.ID == "" {
						t.Fatalf("partial delivery omitted local owner: %s", writer.Body)
					}
					local = delivered.Interaction.ID
					if fact.Usage == nil || fact.Usage.InputTokens != 3 || fact.Usage.OutputTokens != 2 || !fact.UsageObserved || fact.UsageComplete || !fact.BillingUncertain {
						t.Fatalf("partial usage became complete/certain: %+v", fact)
					}
				}
				if local != "" {
					res, _, err := h.Gateway.Resources.ReadInteractionContract(t.Context(), owner, local)
					if err != nil || res.State != "in_progress" || !bytes.Contains(res.Metadata, []byte(`"pending_usage"`)) {
						t.Fatalf("read interruption lost pending original owner: resource=%+v err=%v", res, err)
					}
					var settled int
					if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.attempt_usage_facts WHERE request_id=$1`, original).Scan(&settled); err != nil || settled != 0 {
						t.Fatalf("read interruption settled original generation: count=%d err=%v", settled, err)
					}
				}
				if operation == "retrieval" && (fact.Usage != nil || fact.UsageObserved || fact.GenerationUsageDeferred) {
					t.Fatalf("read cancellation manufactured generation usage: %+v", fact)
				}
				if calls := native.captured()[baseline:]; len(calls) != 1 || calls[0].Method != method {
					t.Fatalf("read interruption repeated provider work: %+v", calls)
				}
			})
		}
	}
}

func (w *geminiStreamDeliveryGate) Write(frame []byte) (int, error) {
	if w.terminal && !bytes.Contains(frame, []byte(`"interaction.completed"`)) {
		return w.body.Write(frame)
	}
	if w.flush {
		w.pending = append([]byte(nil), frame...)
		return len(frame), nil
	}
	return w.deferredDeliveryGate.Write(frame)
}

func (w *geminiStreamDeliveryGate) FlushError() error {
	if !w.flush || w.pending == nil {
		return nil
	}
	w.once.Do(func() {
		w.offered = append([]byte(nil), w.pending...)
		close(w.entered)
	})
	<-w.release
	return io.ErrClosedPipe
}

func TestGeminiInteractionStreamWriterFailureIsCancelled(t *testing.T) {
	for _, operation := range []string{"generation", "retrieval"} {
		for _, terminal := range []bool{false, true} {
			for _, failure := range []string{"write", "flush"} {
				phase := "partial"
				if terminal {
					phase = "terminal"
				}
				t.Run(operation+"/"+phase+"/"+failure, func(t *testing.T) {
					h := newAccessHarness(t)
					provider := newGeminiLifecycleProvider(t)
					provider.partialUsage.Store(true)
					provider.terminalUsage.Store(true)
					slug, key, _ := provisionGeminiLifecycle(t, h, h.owner(), "gemini-interactions", provider)
					baseline := len(provider.captured())
					method, path := http.MethodPost, "/gemini/v1beta/interactions"
					body := `{"model":"` + slug + `","input":"client delivery","store":true,"stream":true}`
					var local, original, owner string
					if operation == "retrieval" {
						local, original, owner = pendingGeminiGeneration(t, h, slug, key)
						method, path, body = http.MethodGet, path+"/"+local+"?stream=true", ""
					}
					sink := &captureSink{}
					h.Gateway.Sink = sink
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					writer := &geminiStreamDeliveryGate{
						deferredDeliveryGate: &deferredDeliveryGate{header: http.Header{}, entered: make(chan struct{}), release: make(chan struct{}), fail: true},
						terminal:             terminal,
						flush:                failure == "flush",
					}
					request := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
					request.Header.Set("X-Goog-Api-Key", key)
					if body != "" {
						request.Header.Set("Content-Type", "application/json")
					}
					done := make(chan struct{})
					t.Cleanup(func() {
						cancel()
						select {
						case <-writer.release:
						default:
							close(writer.release)
						}
						select {
						case <-done:
						case <-time.After(5 * time.Second):
							t.Error("Gemini stream did not finish after cleanup")
						}
					})
					go func() {
						defer close(done)
						h.HTTP.Config.Handler.ServeHTTP(writer, request)
					}()
					select {
					case <-writer.entered:
					case <-done:
						t.Fatalf("stream ended before delivery: status=%d body=%s", writer.status, writer.body.String())
					case <-time.After(15 * time.Second):
						t.Fatal("native event did not reach client delivery")
					}
					cancel()
					close(writer.release)
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Fatal("failed client delivery did not finish")
					}
					event := sink.last()
					if sink.count() != 1 || event.Outcome != "cancelled" || event.Status != 0 || event.ErrorClass != "client_cancelled" || !event.Committed || len(event.Attempts) != 1 {
						t.Fatalf("client write failure misclassified: %+v", event)
					}
					fact := event.Attempts[0]
					if fact.Class != "cancelled" || fact.Interaction == nil || fact.Interaction.ClientState == usage.ClientTerminal || !fact.Committed {
						t.Fatalf("failed pinned delivery changed attempt evidence: %+v", fact)
					}
					if !terminal && (event.FirstByte != nil || fact.Interaction.ClientState != usage.ClientUnobserved) {
						t.Fatalf("failed first delivery claimed client visibility: first_byte=%v client_state=%s", event.FirstByte, fact.Interaction.ClientState)
					}
					wantPhase := usage.UpstreamAccepted
					if terminal {
						wantPhase = usage.UpstreamTerminal
					}
					if fact.Interaction.UpstreamState != wantPhase {
						t.Fatalf("native phase changed: got=%s want=%s", fact.Interaction.UpstreamState, wantPhase)
					}
					if operation == "generation" {
						original, owner = event.AccountingID, event.KeyID
						var offered struct {
							Interaction struct{ ID string } `json:"interaction"`
						}
						_, data, found := bytes.Cut(writer.offered, []byte("data: "))
						if !found || json.Unmarshal(data, &offered) != nil || offered.Interaction.ID == "" {
							t.Fatalf("failed write lost local owner: %s", writer.offered)
						}
						local = offered.Interaction.ID
					}
					res, _, err := h.Gateway.Resources.ReadInteractionContract(t.Context(), owner, local)
					if err != nil {
						t.Fatal(err)
					}
					if terminal {
						input, output := int64(3), int64(2)
						if operation == "retrieval" {
							input, output = 5, 4
						}
						assertGeminiUsageSnapshot(t, h, original, true, input, output, 0)
						if res.State != "completed" || bytes.Contains(res.Metadata, []byte(`"pending_usage"`)) {
							t.Fatalf("validated native terminal stayed pending: state=%s metadata=%s", res.State, res.Metadata)
						}
					} else {
						var count int
						if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.attempt_usage_facts WHERE request_id=$1`, original).Scan(&count); err != nil || count != 0 {
							t.Fatalf("partial native work settled: count=%d err=%v", count, err)
						}
						if res.State != "in_progress" || !bytes.Contains(res.Metadata, []byte(`"pending_usage"`)) {
							t.Fatalf("partial native work lost pending owner: state=%s metadata=%s", res.State, res.Metadata)
						}
					}
					if operation == "retrieval" && (fact.Usage != nil || fact.UsageObserved || fact.GenerationUsageDeferred) {
						t.Fatalf("retained read manufactured generation usage: %+v", fact)
					}
					posts, gets := 0, 0
					for _, call := range provider.captured()[baseline:] {
						if call.Method == http.MethodPost && call.Path == "/v1beta/interactions" {
							posts++
						} else if call.Method == http.MethodGet {
							gets++
						}
					}
					wantGets := 0
					if operation == "retrieval" {
						wantGets = 1
					}
					if posts != 1 || gets != wantGets {
						t.Fatalf("delivery failure repeated work: posts=%d gets=%d", posts, gets)
					}
				})
			}
		}
	}
}
