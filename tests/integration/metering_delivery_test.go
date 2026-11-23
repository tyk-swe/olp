//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

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
		deadline := time.Now().Add(10 * time.Second)
		for {
			var finished bool
			if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.worker_task_health WHERE task='export_delivery' AND successes_total>$1)`, before).Scan(&finished); err != nil {
				t.Fatal(err)
			}
			if finished {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("export worker did not checkpoint")
			}
			time.Sleep(10 * time.Millisecond)
		}
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

	token := "metering-delivery-session"
	if _, err := pool.Exec(ctx, `INSERT INTO olp.sessions(id,user_id,digest,expires_at,browser_hint) VALUES($1,$2,$3,now()+interval '1 hour','test')`, uuid.NewString(), f.ownerID, auth.Digest(secrets.SessionDigest, token)); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	destination.NewAPI(access.New(access.Config{Pool: pool, Installation: installation, Origin: "https://test.example", Auth: auth, Keys: keys}), policy).Register(mux)
	api := httptest.NewServer(mux)
	defer api.Close()
	get := func(sink, attempt string) destination.MeteringDelivery {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, api.URL+"/api/v1/observability/sinks/"+sink+"/metering-deliveries/"+attempt, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "https://test.example")
		req.AddCookie(&http.Cookie{Name: "__Host-olp_session", Value: token})
		response, err := api.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("lookup status=%d body=%s", response.StatusCode, body)
		}
		var delivery destination.MeteringDelivery
		if err := json.Unmarshal(body, &delivery); err != nil {
			t.Fatalf("lookup body=%s err=%v", body, err)
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
