//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/export/destination"
	exportstream "github.com/tyk-swe/olp/internal/export/stream"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
	"github.com/tyk-swe/olp/tests/harness"
)

func TestMeteringDeliveryLookup(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	f := newUsageFixture(t, pool)
	keys, installation := notificationKeys(t, pool)
	auth := secrets.NewAuthKey(make([]byte, 32), installation)
	if err := access.PrepareInstallation(ctx, pool, installation, auth, keys); err != nil {
		t.Fatal(err)
	}
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}

	type seen struct {
		id      string
		eventID string
	}
	acceptedSeen := make(chan seen, 8)
	var acceptCalls atomic.Int32
	accepting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var event struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(body, &event)
		acceptedSeen <- seen{event.ID, r.Header.Get("X-OLP-Event-ID")}
		if acceptCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer accepting.Close()
	droppedSeen := make(chan seen, 8)
	dropping := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var event struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(body, &event)
		droppedSeen <- seen{event.ID, r.Header.Get("X-OLP-Event-ID")}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer dropping.Close()

	sinkAccepted, sinkDropped := uuid.NewString(), uuid.NewString()
	for id, url := range map[string]string{sinkAccepted: accepting.URL, sinkDropped: dropping.URL} {
		if _, err := pool.Exec(ctx, `INSERT INTO olp.export_sinks(id,name,type,destination,streams,format,settings,etag,created_by) VALUES($1,$2,'https',$3,ARRAY['metering'],'cloudevents','{}',$1,$4)`, id, "lookup-"+id[:8], url, f.ownerID); err != nil {
			t.Fatal(err)
		}
	}

	owner := authz.Principal{AllProjects: true}
	lookup := func(sink, attempt string, p authz.Principal) destination.MeteringDelivery {
		t.Helper()
		delivery, err := destination.LookupMeteringDelivery(ctx, pool, p, sink, attempt)
		if err != nil {
			t.Fatal(err)
		}
		return delivery
	}
	persist := func() string {
		t.Helper()
		attempt := f.attempt(1)
		event, err := usage.NewAccountingEvent(f.event(attempt))
		if err != nil {
			t.Fatal(err)
		}
		if outcome, err := ingest.PersistEvent(ctx, pool, event); err != nil || outcome.Outcome != ingest.PersistOutcomePersisted {
			t.Fatalf("persist=%+v err=%v", outcome, err)
		}
		return attempt.ID
	}
	run := func() {
		t.Helper()
		var before int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT successes_total FROM olp.worker_task_health WHERE task='export_delivery'),0)`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		worker := exportstream.Worker{Pool: pool, Keys: keys, Installation: installation, Sender: destination.NewSender(policy, func(string) string { return "" }, installation), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		go func() { defer close(done); worker.Run(runCtx) }()
		defer func() { cancel(); <-done }()
		harness.Eventually(t, "export worker checkpoint", 10*time.Second, func() (bool, string) {
			var finished bool
			if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.worker_task_health WHERE task='export_delivery' AND successes_total>$1)`, before).Scan(&finished); err != nil {
				t.Fatal(err)
			}
			return finished, "export worker has not checkpointed yet"
		})
	}
	maintain := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `SELECT olp.maintain_exports(500)`); err != nil {
			t.Fatal(err)
		}
	}

	attempt1 := persist()
	if got := lookup(sinkAccepted, attempt1, owner); got.Outcome != destination.MeteringPending || got.EventID == nil {
		t.Fatalf("queued lookup = %+v, want pending with an event id", got)
	}

	run()
	if got := lookup(sinkAccepted, attempt1, owner); got.Outcome != destination.MeteringRetrying || got.LastErrorCode == nil || *got.LastErrorCode != "status" {
		t.Fatalf("after 503 lookup = %+v, want retrying with status", got)
	}
	if got := lookup(sinkDropped, attempt1, owner); got.Outcome != destination.MeteringRetrying {
		t.Fatalf("after dropped connection lookup = %+v, want retrying", got)
	}
	select {
	case got := <-droppedSeen:
		if got.id != attempt1 {
			t.Fatalf("dropped receiver saw event %q, want attempt %q", got.id, attempt1)
		}
		if delivery := lookup(sinkDropped, attempt1, owner); delivery.EventID == nil || *delivery.EventID != got.eventID {
			t.Fatalf("lookup event id = %v, receiver saw %q", delivery.EventID, got.eventID)
		}
	default:
		t.Fatal("dropped receiver saw no request")
	}

	if _, err := pool.Exec(ctx, `UPDATE olp.export_pending SET next_attempt_at=now()-interval '1 second' WHERE sink_id=$1`, sinkAccepted); err != nil {
		t.Fatal(err)
	}
	run()
	accepted := lookup(sinkAccepted, attempt1, owner)
	if accepted.Outcome != destination.MeteringAccepted || accepted.SettledAt == nil || accepted.LastErrorCode != nil {
		t.Fatalf("after 2xx lookup = %+v, want accepted", accepted)
	}
	select {
	case got := <-acceptedSeen:
		if got.id != attempt1 {
			t.Fatalf("accepting receiver saw event %q, want %q", got.id, attempt1)
		}
	default:
		t.Fatal("accepting receiver saw no first request")
	}

	if _, err := pool.Exec(ctx, `UPDATE olp.export_records SET occurred_at=now()-interval '35 days' WHERE stream='metering' AND source_id=$1`, attempt1); err != nil {
		t.Fatal(err)
	}
	maintain()
	var records int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM olp.export_records WHERE stream='metering' AND source_id=$1`, attempt1).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if records != 0 {
		t.Fatalf("queue record survived expiry cleanup: %d", records)
	}
	if got := lookup(sinkAccepted, attempt1, owner); got.Outcome != destination.MeteringAccepted || got.EventID == nil || *got.EventID != *accepted.EventID {
		t.Fatalf("accepted receipt after cleanup = %+v, want accepted under the same event id", got)
	}
	if got := lookup(sinkDropped, attempt1, owner); got.Outcome != destination.MeteringDeliveryUnconfirmed {
		t.Fatalf("dropped receipt after expiry = %+v, want delivery_unconfirmed, never expired", got)
	}

	client := managementHTTPAs(t, pool, f.ownerID, func(mux *http.ServeMux, server *access.Server) {
		destination.NewAPI(server, policy).Register(mux)
	})
	get := func(sink, attempt string) destination.MeteringDelivery {
		t.Helper()
		res := expectStatus(t, "lookup", client.call(t, http.MethodGet, "/api/v1/observability/sinks/"+sink+"/metering-deliveries/"+attempt, "", ""), http.StatusOK)
		var delivery destination.MeteringDelivery
		if err := json.Unmarshal(res.body, &delivery); err != nil {
			t.Fatalf("lookup body=%s err=%v", res.body, err)
		}
		return delivery
	}
	if got := get(sinkDropped, attempt1); got.Outcome != destination.MeteringDeliveryUnconfirmed {
		t.Fatalf("HTTP lookup = %+v, want delivery_unconfirmed", got)
	}
	if got := get(sinkAccepted, uuid.NewString()); got.Outcome != destination.MeteringNoRetainedEvidence || got.EventID != nil {
		t.Fatalf("HTTP unknown attempt = %+v, want no retained evidence", got)
	}

	attempt2 := persist()
	if got := lookup(sinkDropped, attempt2, owner); got.Outcome != destination.MeteringPending {
		t.Fatalf("never claimed lookup = %+v, want pending", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE olp.export_records SET occurred_at=now()-interval '35 days' WHERE stream='metering' AND source_id=$1`, attempt2); err != nil {
		t.Fatal(err)
	}
	maintain()
	if got := lookup(sinkDropped, attempt2, owner); got.Outcome != destination.MeteringExpiredBeforeDeliver {
		t.Fatalf("never claimed receipt = %+v, want expired_before_delivery", got)
	}
	select {
	case got := <-droppedSeen:
		t.Fatalf("receiver saw %q for a record that expired before any claim", got.id)
	default:
	}

	var gaps int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM olp.export_gaps WHERE sink_id=$1`, sinkAccepted).Scan(&gaps); err != nil {
		t.Fatal(err)
	}
	if gaps == 0 {
		t.Fatal("expected an aggregate gap from the expired records")
	}
	if _, err := pool.Exec(ctx, `UPDATE olp.metering_delivery_receipts SET settled_at=now()-interval '35 days' WHERE attempt_id=$1`, attempt1); err != nil {
		t.Fatal(err)
	}
	maintain()
	if got := lookup(sinkAccepted, attempt1, owner); got.Outcome != destination.MeteringNoRetainedEvidence || got.EventID != nil {
		t.Fatalf("aged out receipt = %+v, want no retained evidence despite the aggregate gap", got)
	}

	outsider := authz.Principal{Projects: map[string]string{uuid.NewString(): "member"}}
	if _, err := destination.LookupMeteringDelivery(ctx, pool, outsider, sinkAccepted, attempt2); err == nil {
		t.Fatal("outsider lookup succeeded, want not found")
	} else if problem, known := httpkit.ProblemFor(err); !known || problem.Status != http.StatusNotFound {
		t.Fatalf("outsider lookup error = %v, want 404", err)
	}

	sinkScoped := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO olp.export_sinks(id,name,type,destination,streams,format,settings,etag,created_by,project_id) VALUES($1,'scoped','https',$2,ARRAY['metering'],'cloudevents','{}',$1,$3,$4)`, sinkScoped, accepting.URL, f.ownerID, f.projectID); err != nil {
		t.Fatal(err)
	}
	attempt3 := persist()
	member := authz.Principal{Projects: map[string]string{f.projectID: "member"}}
	if got := lookup(sinkScoped, attempt3, member); got.Outcome != destination.MeteringPending {
		t.Fatalf("member lookup = %+v, want pending", got)
	}
	otherProject := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO olp.projects(id,name,etag,created_by) VALUES($1,'other consumer',$1,$2)`, otherProject, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE olp.export_records SET project_id=$2 WHERE stream='metering' AND source_id=$1`, attempt3, otherProject); err != nil {
		t.Fatal(err)
	}
	if got := lookup(sinkScoped, attempt3, member); got.Outcome != destination.MeteringNoRetainedEvidence || got.EventID != nil {
		t.Fatalf("foreign consumer lookup = %+v, want no retained evidence and no existence hint", got)
	}
	if got := lookup(sinkScoped, attempt3, owner); got.Outcome != destination.MeteringPending {
		t.Fatalf("owner lookup after consumer move = %+v, want pending", got)
	}
}

func TestExportRetryPreservesSettings(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"source", "default_source", "name", "stripe"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			pool, _ := harness.Database(t)
			f := newUsageFixture(t, pool)
			var calls atomic.Int32
			type delivery struct {
				body []byte
				key  string
			}
			bodies := make(chan delivery, 4)
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				bodies <- delivery{body, r.Header.Get("Idempotency-Key")}
				if calls.Add(1) == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer receiver.Close()
			sink := uuid.NewString()
			keys, installation := notificationKeys(t, pool)
			auth := secrets.NewAuthKey(make([]byte, 32), installation)
			if err := access.PrepareInstallation(t.Context(), pool, installation, auth, keys); err != nil {
				t.Fatal(err)
			}
			settings, patch, sinkType, format, credential := func() (string, string, string, string, *string) {
				switch change {
				case "stripe":
					id := uuid.NewString()
					if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
						return keys.Store(t.Context(), tx, installation, id, secrets.SinkCredential, []byte(`{"api_key":"sk_test_fixture"}`), nil)
					}); err != nil {
						t.Fatal(err)
					}
					return fmt.Sprintf(`{"event_name":"original","value":"requests","customers":{%q:"cus_original"}}`, f.projectID),
						fmt.Sprintf(`{"settings":{"event_name":"updated","value":"input_tokens","customer_key":"customer","value_key":"amount","customers":{%q:"cus_updated"}}}`, f.projectID),
						"stripe_meter", "stripe", &id
				case "name":
					return `{"source":"/original"}`, `{"name":"renamed"}`, "https", "cloudevents", nil
				case "default_source":
					return `{}`, `{"settings":{"source":"/changed"}}`, "https", "cloudevents", nil
				default:
					return `{"source":"/original"}`, `{"settings":{"source":"/changed"}}`, "https", "cloudevents", nil
				}
			}()
			if _, err := pool.Exec(t.Context(), `INSERT INTO olp.export_sinks(id,name,type,destination,streams,format,settings,etag,created_by,credential_id) VALUES($1,'retry-settings',$5,$2,ARRAY['metering'],$6,$4,$1,$3,$7)`, sink, receiver.URL, f.ownerID, settings, sinkType, format, credential); err != nil {
				t.Fatal(err)
			}
			event, err := usage.NewAccountingEvent(f.event(f.attempt(1)))
			if err != nil {
				t.Fatal(err)
			}
			if outcome, err := ingest.PersistEvent(t.Context(), pool, event); err != nil || outcome.Outcome != ingest.PersistOutcomePersisted {
				t.Fatalf("persist result=%+v error=%v", outcome, err)
			}
			policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
			run := func() {
				var before int64
				if err := pool.QueryRow(t.Context(), `SELECT COALESCE((SELECT successes_total FROM olp.worker_task_health WHERE task='export_delivery'),0)`).Scan(&before); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				worker := exportstream.Worker{Pool: pool, Keys: keys, Installation: installation, Sender: destination.NewSender(policy, func(string) string { return "" }, installation), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
				go func() { defer close(done); worker.Run(ctx) }()
				defer func() { cancel(); <-done }()
				harness.Eventually(t, "export worker checkpoint", 5*time.Second, func() (bool, string) {
					var finished bool
					if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp.worker_task_health WHERE task='export_delivery' AND successes_total>$1)`, before).Scan(&finished); err != nil {
						t.Fatal(err)
					}
					return finished, "export worker has not checkpointed yet"
				})
			}
			armRetry := func() {
				if _, err := pool.Exec(t.Context(), `UPDATE olp.export_pending SET next_attempt_at=now()-interval '1 second' WHERE sink_id=$1`, sink); err != nil {
					t.Fatal(err)
				}
			}
			if change == "stripe" {
				otherProject := uuid.NewString()
				if _, err := pool.Exec(t.Context(), `INSERT INTO olp.projects(id,name,etag,created_by) VALUES($1,'another project',$1,$2)`, otherProject, f.ownerID); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(t.Context(), `UPDATE olp.export_sinks SET settings=jsonb_set(settings,'{customers}',jsonb_build_object($2::text,'cus_other')) WHERE id=$1`, sink, otherProject); err != nil {
					t.Fatal(err)
				}
				run()
				var code *string
				if err := pool.QueryRow(t.Context(), `SELECT last_error_code FROM olp.export_pending WHERE sink_id=$1`, sink).Scan(&code); err != nil {
					t.Fatal(err)
				}
				if len(bodies) != 0 || code == nil || *code != "unmapped" {
					t.Fatal("the queued event was not deferred while its project was unmapped")
				}
				if _, err := pool.Exec(t.Context(), `UPDATE olp.export_sinks SET settings=$2 WHERE id=$1`, sink, settings); err != nil {
					t.Fatal(err)
				}
				armRetry()
			}
			run()
			client := managementHTTPAs(t, pool, f.ownerID, func(mux *http.ServeMux, server *access.Server) {
				destination.NewAPI(server, policy).Register(mux)
			})
			expectStatus(t, "PATCH export sink", client.call(t, http.MethodPatch, "/api/v1/observability/sinks/"+sink, `"`+sink+`"`, patch), http.StatusOK)

			var code *string
			var delivered bool
			if err := pool.QueryRow(t.Context(), `SELECT last_error_code,delivered_at IS NOT NULL FROM olp.export_pending WHERE sink_id=$1`, sink).Scan(&code, &delivered); err != nil {
				t.Fatal(err)
			}
			if delivered || code == nil || *code != "status" {
				t.Fatalf("failed HTTP attempt evidence: delivered=%t code=%v", delivered, code)
			}
			armRetry()
			run()
			if len(bodies) != 2 {
				t.Fatalf("deliveries=%d, want2", len(bodies))
			}
			first, second := <-bodies, <-bodies
			if !bytes.Equal(first.body, second.body) || first.key != second.key {
				t.Fatalf("retry changed event: first=%s retry=%s", first.body, second.body)
			}
			nextAttempt := f.attempt(1)
			next, err := usage.NewAccountingEvent(f.event(nextAttempt))
			if err != nil {
				t.Fatal(err)
			}
			if outcome, err := ingest.PersistEvent(t.Context(), pool, next); err != nil || outcome.Outcome != ingest.PersistOutcomePersisted {
				t.Fatalf("next persist=%+v err=%v", outcome, err)
			}
			run()
			if len(bodies) != 1 {
				t.Fatalf("new deliveries=%d, want1", len(bodies))
			}
			third := <-bodies
			if change == "stripe" {
				original, beforeErr := url.ParseQuery(string(first.body))
				updated, afterErr := url.ParseQuery(string(third.body))
				if beforeErr != nil || afterErr != nil || first.key == "" || third.key == first.key || original.Get("event_name") != "original" || original.Get("payload[stripe_customer_id]") != "cus_original" || updated.Get("event_name") != "updated" || updated.Get("payload[customer]") != "cus_updated" || updated.Get("payload[amount]") == "" {
					t.Fatalf("new event did not use the edited meter: original=%s updated=%s", first.body, third.body)
				}
				return
			}
			var body struct {
				Source string `json:"source"`
				ID     string `json:"id"`
			}
			if err := json.Unmarshal(third.body, &body); err != nil {
				t.Fatal(err)
			}
			want := "/original"
			if change != "name" {
				want = "/changed"
			}
			if body.Source != want || body.ID != nextAttempt.ID {
				t.Fatalf("new event uses wrong settings: %s", third.body)
			}
			if calls.Load() != 3 {
				t.Fatalf("calls=%d", calls.Load())
			}
		})
	}
}

func TestStreamExportSuccessClearsFailureEvidence(t *testing.T) {
	t.Parallel()
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry=%t", retry), func(t *testing.T) {
			t.Parallel()
			pool, _ := harness.Database(t)
			f := newUsageFixture(t, pool)
			var calls atomic.Int32
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if calls.Add(1) == 1 && retry {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer receiver.Close()
			sink := uuid.NewString()
			if _, err := pool.Exec(t.Context(), `INSERT INTO olp.export_sinks(id,name,type,destination,streams,format,etag,created_by) VALUES($1,'success-proof','https',$2,ARRAY['requests'],'json',$1,$3)`, sink, receiver.URL, f.ownerID); err != nil {
				t.Fatal(err)
			}
			event, err := usage.NewAccountingEvent(f.event(f.attempt(1)))
			if err != nil {
				t.Fatal(err)
			}
			if outcome, err := ingest.PersistEvent(t.Context(), pool, event); err != nil || outcome.Outcome != ingest.PersistOutcomePersisted {
				t.Fatalf("persist result=%+v error=%v", outcome, err)
			}
			policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
			run := func() {
				var before int64
				if err := pool.QueryRow(t.Context(), `SELECT COALESCE((SELECT successes_total FROM olp.worker_task_health WHERE task='export_delivery'),0)`).Scan(&before); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				worker := exportstream.Worker{Pool: pool, Sender: destination.NewSender(policy, func(string) string { return "" }, "synthetic-installation"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
				go func() { defer close(done); worker.Run(ctx) }()
				defer func() { cancel(); <-done }()
				harness.Eventually(t, "export worker checkpoint", 5*time.Second, func() (bool, string) {
					var finished bool
					if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp.worker_task_health WHERE task='export_delivery' AND successes_total>$1)`, before).Scan(&finished); err != nil {
						t.Fatal(err)
					}
					return finished, "export worker has not checkpointed yet"
				})
			}
			run()
			if retry {
				var code *string
				var delivered bool
				if err := pool.QueryRow(t.Context(), `SELECT last_error_code,delivered_at IS NOT NULL FROM olp.export_pending WHERE sink_id=$1`, sink).Scan(&code, &delivered); err != nil {
					t.Fatal(err)
				}
				if delivered || code == nil || *code != "status" {
					t.Fatalf("failed HTTP attempt evidence: delivered=%t code=%v", delivered, code)
				}
				if _, err := pool.Exec(t.Context(), `UPDATE olp.export_pending SET next_attempt_at=now()-interval '1 second' WHERE sink_id=$1`, sink); err != nil {
					t.Fatal(err)
				}
				run()
			}
			var code *string
			var delivered bool
			var successes, failures int64
			if err := pool.QueryRow(t.Context(), `SELECT p.last_error_code,p.delivered_at IS NOT NULL,c.delivered_total,c.failed_total FROM olp.export_pending p JOIN olp.export_cursors c ON c.sink_id=p.sink_id AND c.stream='requests' WHERE p.sink_id=$1`, sink).Scan(&code, &delivered, &successes, &failures); err != nil {
				t.Fatal(err)
			}
			errorCode := "<null>"
			if code != nil {
				errorCode = *code
			}
			t.Logf("HTTP calls=%d delivered=%t successes=%d failures=%d last_error_code=%s", calls.Load(), delivered, successes, failures, errorCode)
			expectedFailures := int64(0)
			if retry {
				expectedFailures = 1
			}
			if !delivered || successes != 1 || failures != expectedFailures || code != nil {
				t.Fatalf("successful delivery retained false error evidence: delivered=%t successes=%d failures=%d last_error_code=%s", delivered, successes, failures, errorCode)
			}
		})
	}
}
