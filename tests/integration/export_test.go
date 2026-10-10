//go:build integration

package integration_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/export"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

type exportReceiver struct {
	t          *testing.T
	mu         sync.Mutex
	envelopes  []map[string]any
	signatures []string
	secret     string
	status     int
	block      chan struct{}
}

func (r *exportReceiver) setStatus(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

func newExportReceiver(t *testing.T, secret string, status int) (*exportReceiver, *httptest.Server) {
	r := &exportReceiver{t: t, secret: secret, status: status}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		block := r.block
		status := r.status
		r.mu.Unlock()
		if block != nil {
			<-block
		}
		if status != 200 && status != 204 {
			r.mu.Lock()
			var envelope map[string]any
			if json.Unmarshal(body, &envelope) == nil {
				r.envelopes = append(r.envelopes, envelope)
			}
			r.mu.Unlock()
			w.WriteHeader(status)
			return
		}
		if secret != "" {
			sum := hmac.New(sha256.New, []byte(secret))
			sum.Write(body)
			if req.Header.Get("X-OLP-Signature") != "sha256="+hex.EncodeToString(sum.Sum(nil)) {
				t.Error("envelope signature does not cover the sent bytes")
				w.WriteHeader(400)
				return
			}
		}
		var envelope map[string]any
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Errorf("invalid envelope: %v", err)
			w.WriteHeader(400)
			return
		}
		r.mu.Lock()
		r.envelopes = append(r.envelopes, envelope)
		r.signatures = append(r.signatures, req.Header.Get("X-OLP-Signature"))
		r.mu.Unlock()
		w.WriteHeader(204)
	}))
	t.Cleanup(server.Close)
	return r, server
}

func (r *exportReceiver) received() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, len(r.envelopes))
	copy(out, r.envelopes)
	return out
}

func exportWorker(t *testing.T, pool *pgxpool.Pool, installation string, ring *secrets.KeyRing, policy *egress.Policy) *export.Worker {
	t.Helper()
	sender := export.NewSender(policy)
	sender.Installation = installation
	return &export.Worker{Pool: pool, Keys: ring, Installation: installation, Sender: sender, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func loopbackPolicy() *egress.Policy {
	return &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
}

func awaitEnvelope(t *testing.T, r *exportReceiver, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := r.received(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d envelopes", n)
	return nil
}

func enqueueFixture(t *testing.T, pool *pgxpool.Pool, stream string, project *string, route, outcome string, payload map[string]any) string {
	t.Helper()
	source := uuid.NewString()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `SELECT olp.enqueue_export($1,$2::uuid,$3::uuid,$4::text,$5::text,$6,$7::jsonb)`, stream, source, project, route, outcome, time.Now(), body); err != nil {
		t.Fatal(err)
	}
	return source
}

func pendingCount(t *testing.T, pool *pgxpool.Pool, sinkID string) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.export_pending WHERE sink_id=$1 AND delivered_at IS NULL`, sinkID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestExportSinkDeliveryEndToEnd(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	receiver, server := newExportReceiver(t, "0123456789abcdef0123456789abcdef", 204)
	created := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "receiver", "type": "https", "destination": server.URL, "streams": []string{"requests", "audit"}, "format": "json",
		"credential": map[string]any{"signing_secret": "0123456789abcdef0123456789abcdef"},
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	sinkID := created["export_sink_id"].(string)
	if created["credential_configured"] != true {
		t.Fatal("credential_configured not reported")
	}
	if created["credential"] != nil || created["credential_id"] != nil {
		t.Fatalf("credential leaked: %v", created)
	}
	detail := h.want(owner, "GET", "/api/v1/observability/sinks/"+sinkID, nil, nil, 200)
	if streams := detail["status"].([]any); len(streams) != 2 {
		t.Fatalf("status streams %v", detail["status"])
	}
	var count int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.secrets s JOIN olp.export_sinks k ON k.credential_id=s.id WHERE k.id=$1 AND s.purpose='sink_credential'`, sinkID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("credential not sealed under the sink purpose: %v %d", err, count)
	}
	enqueueFixture(t, h.Pool, "requests", nil, "team-chat", "success", map[string]any{"sentinel": "fixture-request"})
	audit := uuid.NewString()
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.audit(id,action,resource_type,resource_id,outcome,user_agent_family) VALUES($1,'fixture.action','fixture',$2,'success','other')`, uuid.NewString(), audit); err != nil {
		t.Fatal(err)
	}
	w := exportWorker(t, h.Pool, h.Server.Installation, h.Ring2(), loopbackPolicy())
	if err := w.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	envs := awaitEnvelope(t, receiver, 2)
	var streams []string
	for _, env := range envs {
		streams = append(streams, env["stream"].(string))
	}
	if !contains(streams, "requests") || !contains(streams, "audit") {
		t.Fatalf("streams %v", streams)
	}
	if pendingCount(t, h.Pool, sinkID) != 0 {
		t.Fatal("delivered records stayed pending")
	}
	var delivered, failed int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT COALESCE(sum(delivered_total),0),COALESCE(sum(failed_total),0) FROM olp.export_cursors WHERE sink_id=$1`, sinkID).Scan(&delivered, &failed); err != nil || delivered < 2 || failed != 0 {
		t.Fatalf("cursor delivered=%d failed=%d err=%v", delivered, failed, err)
	}
}

func (h *accessHarness) Ring2() *secrets.KeyRing {
	ring, err := secrets.ParseRing([]byte(h.Ring))
	if err != nil {
		h.t.Fatal(err)
	}
	return ring
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestExportDeliveryRetryBackoffAndDuplicateID(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	receiver, server := newExportReceiver(t, "", 503)
	created := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "flaky", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	sinkID := created["export_sink_id"].(string)
	enqueueFixture(t, h.Pool, "requests", nil, "team-chat", "success", map[string]any{"id": "record-1"})
	w := exportWorker(t, h.Pool, h.Server.Installation, h.Ring2(), loopbackPolicy())
	if err := w.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	var attempts int64
	var nextAttempt time.Time
	if err := h.Pool.QueryRow(t.Context(), `SELECT attempts,next_attempt_at FROM olp.export_pending WHERE sink_id=$1 AND delivered_at IS NULL`, sinkID).Scan(&attempts, &nextAttempt); err != nil {
		t.Fatal("failed record was removed instead of retried")
	}
	if attempts != 1 || time.Until(nextAttempt) < 45*time.Second {
		t.Fatalf("attempts=%d next=%v", attempts, nextAttempt)
	}
	var lastError *string
	if err := h.Pool.QueryRow(t.Context(), `SELECT last_error_code FROM olp.export_pending WHERE sink_id=$1`, sinkID).Scan(&lastError); err != nil || lastError == nil || *lastError != "status" {
		t.Fatalf("error code %v", lastError)
	}
	var cursor *string
	if err := h.Pool.QueryRow(t.Context(), `SELECT cursor FROM olp.export_cursors WHERE sink_id=$1 AND stream='requests'`, sinkID).Scan(&cursor); err != nil || cursor != nil {
		t.Fatalf("cursor advanced on failure: %v", cursor)
	}
	// The claim lease expires; the retried attempt posts the same immutable
	// event id, so the receiver can deduplicate at-least-once delivery.
	receiver.setStatus(204)
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.export_pending SET next_attempt_at=now() WHERE sink_id=$1`, sinkID); err != nil {
		t.Fatal(err)
	}
	w = exportWorker(t, h.Pool, h.Server.Installation, h.Ring2(), loopbackPolicy())
	if err := w.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	envs := awaitEnvelope(t, receiver, 2)
	if envs[0]["event_id"].(string) != envs[1]["event_id"].(string) {
		t.Fatalf("retried delivery changed event id: %s != %s", envs[0]["event_id"], envs[1]["event_id"])
	}
	if pendingCount(t, h.Pool, sinkID) != 0 {
		t.Fatal("redelivery did not ACK")
	}
}

func TestExportSinkStatusPendingErrorCodes(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	receiver, server := newExportReceiver(t, "", 503)
	created := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "status-sink", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	sinkID := created["export_sink_id"].(string)
	enqueueFixture(t, h.Pool, "requests", nil, "team-chat", "success", map[string]any{"id": "status-record"})
	w := exportWorker(t, h.Pool, h.Server.Installation, h.Ring2(), loopbackPolicy())
	if err := w.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	list := h.want(owner, "GET", "/api/v1/observability/sinks", nil, nil, 200)
	var entry map[string]any
	for _, item := range list["items"].([]any) {
		if item.(map[string]any)["export_sink_id"] == sinkID {
			entry = item.(map[string]any)
		}
	}
	if entry == nil {
		t.Fatal("sink missing from list")
	}
	statuses := entry["status"].([]any)
	if len(statuses) != 1 {
		t.Fatalf("status streams %v", entry["status"])
	}
	st := statuses[0].(map[string]any)
	if st["pending"].(float64) != 1 {
		t.Fatalf("pending %v", st)
	}
	codes, _ := st["pending_error_codes"].([]any)
	if len(codes) != 1 || codes[0] != "status" {
		t.Fatalf("pending_error_codes %v", st)
	}
	receiver.setStatus(204)
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.export_pending SET next_attempt_at=now() WHERE sink_id=$1`, sinkID); err != nil {
		t.Fatal(err)
	}
	w = exportWorker(t, h.Pool, h.Server.Installation, h.Ring2(), loopbackPolicy())
	if err := w.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	awaitEnvelope(t, receiver, 2)
	list = h.want(owner, "GET", "/api/v1/observability/sinks", nil, nil, 200)
	for _, item := range list["items"].([]any) {
		candidate := item.(map[string]any)
		if candidate["export_sink_id"] != sinkID {
			continue
		}
		st = candidate["status"].([]any)[0].(map[string]any)
		if st["pending"].(float64) != 0 {
			t.Fatalf("acknowledged record still pending: %v", st)
		}
		if codes := st["pending_error_codes"].([]any); len(codes) != 0 {
			t.Fatalf("pending_error_codes after ack: %v", codes)
		}
	}
}

func TestExportFiltersAndLateFacts(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fixtureRoute(t, h, "team-chat")
	receiver, server := newExportReceiver(t, "", 204)
	created := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "filtered", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
		"filter": map[string]any{"route": "team-chat"},
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	sinkID := created["export_sink_id"].(string)
	enqueueFixture(t, h.Pool, "requests", nil, "other-route", "success", map[string]any{"id": "excluded"})
	enqueueFixture(t, h.Pool, "requests", nil, "team-chat", "success", map[string]any{"id": "included"})
	w := exportWorker(t, h.Pool, h.Server.Installation, h.Ring2(), loopbackPolicy())
	if err := w.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	envs := awaitEnvelope(t, receiver, 1)
	if len(envs) != 1 || envs[0]["route"].(string) != "team-chat" {
		t.Fatalf("filter did not exclude: %v", envs)
	}
	// A record committed after the cursor advanced is still delivered: no
	// cursor position can skip a committed pending entry.
	enqueueFixture(t, h.Pool, "requests", nil, "team-chat", "failure", map[string]any{"id": "late"})
	if err := w.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	envs = awaitEnvelope(t, receiver, 2)
	if envs[1]["outcome"].(string) != "failure" {
		t.Fatalf("late record not delivered: %v", envs)
	}
	if pendingCount(t, h.Pool, sinkID) != 0 {
		t.Fatal("late record stayed pending")
	}
}

func TestExportRetentionRecordsGaps(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	receiver, server := newExportReceiver(t, "", 204)
	_ = receiver
	created := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "stalled", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	sinkID := created["export_sink_id"].(string)
	source := enqueueFixture(t, h.Pool, "requests", nil, "team-chat", "success", map[string]any{"id": "expired"})
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.export_records SET occurred_at=now()-interval '40 days',queued_at=now()-interval '40 days' WHERE stream='requests' AND source_id=$1::uuid`, source); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.export_pending SET next_attempt_at=now()+interval '1 hour' WHERE sink_id=$1`, sinkID); err != nil {
		t.Fatal(err)
	}
	w := exportWorker(t, h.Pool, h.Server.Installation, h.Ring2(), loopbackPolicy())
	if err := w.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	var gaps, records int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT record_count FROM olp.export_gaps WHERE sink_id=$1 AND stream='requests'`, sinkID).Scan(&records); err != nil {
		t.Fatal("expired record produced no durable gap")
	}
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.export_pending WHERE sink_id=$1`, sinkID).Scan(&gaps); err != nil || gaps != 0 {
		t.Fatal("expired record stayed queued")
	}
	if pendingCount(t, h.Pool, sinkID) != 0 {
		t.Fatal("expired record stayed pending")
	}
}

func TestExportMultipleSinksDeliverEach(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	receiverA, serverA := newExportReceiver(t, "", 204)
	receiverB, serverB := newExportReceiver(t, "", 204)
	for i, server := range []*httptest.Server{serverA, serverB} {
		h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
			"name": "multi-" + string(rune('a'+i)), "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
		}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	}
	enqueueFixture(t, h.Pool, "requests", nil, "team-chat", "success", map[string]any{"id": "fan-out"})
	w := exportWorker(t, h.Pool, h.Server.Installation, h.Ring2(), loopbackPolicy())
	if err := w.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	awaitEnvelope(t, receiverA, 1)
	awaitEnvelope(t, receiverB, 1)
}

func TestProjectAuditSinkScopesAuditStream(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "audit-scope"}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	projectID := project["id"].(string)
	receiver, server := newExportReceiver(t, "", 204)
	h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "project-audit", "project_id": projectID, "type": "https", "destination": server.URL, "streams": []string{"audit"}, "format": "json",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	// An audit row for the project is exported; other-project and installation
	// rows are not.
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.audit(id,action,resource_type,resource_id,outcome,user_agent_family,project_id) VALUES($1,'fixture.own','fixture',$2,'success','other',$3)`, uuid.NewString(), uuid.NewString(), projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.audit(id,action,resource_type,resource_id,outcome,user_agent_family,project_id) VALUES($1,'fixture.other','fixture',$2,'success','other',$3)`, uuid.NewString(), uuid.NewString(), uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.audit(id,action,resource_type,resource_id,outcome,user_agent_family) VALUES($1,'fixture.installation','fixture',$2,'success','other')`, uuid.NewString(), uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	w := exportWorker(t, h.Pool, h.Server.Installation, h.Ring2(), loopbackPolicy())
	if err := w.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	envs := awaitEnvelope(t, receiver, 1)
	own := false
	for _, env := range envs {
		data := env["data"].(map[string]any)
		if data["project_id"] != nil && data["project_id"].(string) != projectID {
			t.Fatalf("project sink received foreign audit: %v", env)
		}
		if data["action"].(string) == "fixture.own" {
			own = true
		}
		if data["action"].(string) == "fixture.other" || data["action"].(string) == "fixture.installation" {
			t.Fatalf("project sink received out-of-scope audit: %v", env)
		}
	}
	if !own {
		t.Fatalf("project sink missed its audit: %v", envs)
	}
}

func fixtureRoute(t *testing.T, h *accessHarness, slug string) {
	t.Helper()
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.routes(id,slug,created_by,latest_revision,latest_revision_id,etag) VALUES(gen_random_uuid(),$1,(SELECT id FROM olp.users LIMIT 1),1,gen_random_uuid(),gen_random_uuid())`, slug); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureConfigurationAndPolicyOwnerOnly(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fixtureRoute(t, h, "team-chat")
	operator := h.invite(owner, "operator@example.com", "operator")
	detail := h.want(owner, "GET", "/api/v1/observability/capture", nil, nil, 200)
	if detail["enabled"] != false {
		t.Fatal("capture defaults to disabled")
	}
	etag := detail["etag"].(string)
	h.want(operator, "PATCH", "/api/v1/observability/capture", map[string]any{"enabled": true}, map[string]string{"If-Match": `"` + etag + `"`}, 403)
	h.want(owner, "POST", "/api/v1/observability/capture-policies", map[string]any{
		"sink": uuid.NewString(), "sample_ratio": "1", "include": []string{"output"}, "max_bytes": 1048576, "project_id": uuid.NewString(),
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 422)
	h.want(owner, "PATCH", "/api/v1/observability/capture", map[string]any{"enabled": true}, map[string]string{"If-Match": `"` + etag + `"`}, 200)
	receiver, server := newExportReceiver(t, "", 204)
	sink := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "capture-destination", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	policy := h.want(owner, "POST", "/api/v1/observability/capture-policies", map[string]any{
		"sink": sink["export_sink_id"], "sample_ratio": "1", "include": []string{"output"}, "max_bytes": 1048576, "project_id": nil, "route_slug": "team-chat",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	if policy["id"] == nil {
		t.Fatalf("policy not created: %v", policy)
	}
	manager := &export.Manager{Pool: h.Pool, Keys: h.Ring2(), Installation: h.Server.Installation, Sender: export.NewSender(loopbackPolicy()), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	manager.Sender.Installation = h.Server.Installation
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { manager.Run(ctx); close(done) }()
	time.Sleep(250 * time.Millisecond)
	collector := manager.Resolve("", "team-chat", "", "", uuid.NewString())
	if collector == nil {
		cancel()
		t.Fatal("request not sampled under an enabled policy")
	}
	collector.CaptureText("output", "capture-sentinel-content")
	if !collector.Finish(export.CaptureIdentity{Route: "team-chat", Outcome: "success"}, time.Now()) {
		cancel()
		t.Fatal("record not queued")
	}
	envs := awaitEnvelope(t, receiver, 1)
	env := envs[0]
	if env["stream"].(string) != "captures" {
		t.Fatalf("envelope stream %v", env["stream"])
	}
	data := env["data"].(map[string]any)
	if data["output"].(string) != "capture-sentinel-content" {
		t.Fatalf("captured output missing: %v", data)
	}
	if data["input"] != nil {
		t.Fatalf("unrequested input captured: %v", data)
	}
	var audited string
	if err := h.Pool.QueryRow(t.Context(), `SELECT outcome FROM olp.audit WHERE action='capture.delivered' ORDER BY occurred_at DESC LIMIT 1`).Scan(&audited); err != nil || audited != "success" {
		t.Fatalf("capture delivery not audited: %v %v", audited, err)
	}
	var leaked int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.requests r WHERE r.policy_decisions::text LIKE '%capture-sentinel%'`).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatal("captured content leaked into requests")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("capture queue did not drain")
	}
}

func TestCaptureStalePolicyDropsQueuedRecord(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fixtureRoute(t, h, "team-chat")
	h.want(owner, "PATCH", "/api/v1/observability/capture", map[string]any{"enabled": true}, etagHeader(h.want(owner, "GET", "/api/v1/observability/capture", nil, nil, 200)), 200)
	receiver, server := newExportReceiver(t, "", 204)
	sink := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "capture-stale", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	policy := h.want(owner, "POST", "/api/v1/observability/capture-policies", map[string]any{
		"sink": sink["export_sink_id"], "sample_ratio": "1", "include": []string{"output"}, "max_bytes": 1048576, "route_slug": "team-chat",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	manager := &export.Manager{Pool: h.Pool, Keys: h.Ring2(), Installation: h.Server.Installation, Sender: export.NewSender(loopbackPolicy()), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	block := make(chan struct{})
	receiver.mu.Lock()
	receiver.block = block
	receiver.mu.Unlock()
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(block) }) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { manager.Run(ctx); close(done) }()
	time.Sleep(250 * time.Millisecond)
	collector := manager.Resolve("", "team-chat", "", "", uuid.NewString())
	if collector == nil {
		t.Fatal("not sampled")
	}
	collector.CaptureText("output", "fresh-sentinel")
	if !collector.Finish(export.CaptureIdentity{Route: "team-chat", Outcome: "success"}, time.Now()) {
		t.Fatal("not queued")
	}
	time.Sleep(100 * time.Millisecond)
	// Updating the policy bumps its etag while the first record is mid-send;
	// the second queued record resolves against the old etag and is dropped.
	policyID := policy["id"].(string)
	h.want(owner, "PATCH", "/api/v1/observability/capture-policies/"+policyID, map[string]any{"sample_ratio": "0.5"}, map[string]string{"If-Match": `"` + policy["etag"].(string) + `"`}, 200)
	stale := manager.Resolve("", "team-chat", "", "", uuid.NewString())
	if stale == nil {
		t.Fatal("second request not sampled")
	}
	stale.CaptureText("output", "stale-sentinel")
	if !stale.Finish(export.CaptureIdentity{Route: "team-chat", Outcome: "success"}, time.Now()) {
		t.Fatal("second record not queued")
	}
	releaseOnce.Do(func() { close(block) })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, dropped, _, _ := manager.Metrics(); dropped >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	envs := receiver.received()
	var sawStale bool
	for _, env := range envs {
		if env["data"].(map[string]any)["output"] == "stale-sentinel" {
			sawStale = true
		}
	}
	if sawStale {
		t.Fatalf("stale record delivered: %v", envs)
	}
	var outcome string
	if err := h.Pool.QueryRow(t.Context(), `SELECT outcome FROM olp.audit WHERE action='capture.dropped' ORDER BY occurred_at DESC LIMIT 1`).Scan(&outcome); err != nil {
		t.Fatal("stale drop not audited")
	}
	if _, _, dropped, _, _ := manager.Metrics(); dropped < 1 {
		t.Fatal("stale drop not counted")
	}
}

func TestExportPassRotationAvoidsStarvation(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	block := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(block) }) })
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-block
		w.WriteHeader(204)
	}))
	t.Cleanup(blocked.Close)
	fast, fastServer := newExportReceiver(t, "", 204)
	var sinkIDs []string
	for _, name := range []string{"one", "two"} {
		created := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
			"name": name, "type": "https", "destination": fastServer.URL, "streams": []string{"requests"}, "format": "json",
		}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
		sinkIDs = append(sinkIDs, created["export_sink_id"].(string))
	}
	sort.Strings(sinkIDs)
	slowID, fastID := sinkIDs[0], sinkIDs[1]
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.export_sinks SET destination=$1 WHERE id=$2`, blocked.URL, slowID); err != nil {
		t.Fatal(err)
	}
	enqueueFixture(t, h.Pool, "requests", nil, "team-chat", "success", map[string]any{"id": "slow-record"})
	enqueueFixture(t, h.Pool, "requests", nil, "team-chat", "success", map[string]any{"id": "fast-record"})
	w := exportWorker(t, h.Pool, h.Server.Installation, h.Ring2(), loopbackPolicy())
	w.PassBudget = 350 * time.Millisecond
	started := time.Now()
	if err := w.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("pass exceeded its budget: %v", elapsed)
	}
	if got := fast.received(); len(got) != 0 {
		t.Fatalf("pass one reached past the stalled pair: %v", got)
	}
	if err := w.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	awaitEnvelope(t, fast, 2)
	if pendingCount(t, h.Pool, fastID) != 0 {
		t.Fatal("fast sink's records stayed pending")
	}
	var slowPending int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.export_pending WHERE sink_id=$1 AND delivered_at IS NULL`, slowID).Scan(&slowPending); err != nil || slowPending == 0 {
		t.Fatal("stalled records must stay pending for retry, not be dropped")
	}
	releaseOnce.Do(func() { close(block) })
}

func TestExportFilterPatchPreservesFacts(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fixtureRoute(t, h, "team-chat")
	fixtureRoute(t, h, "other-route")
	receiver, server := newExportReceiver(t, "", 204)
	sink := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "unfiltered", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	if got := sink["filter"]; got != nil && len(got.(map[string]any)) != 0 {
		t.Fatalf("unfiltered sink persisted a non-empty filter: %v", got)
	}
	sinkID := sink["export_sink_id"].(string)
	etag := sink["etag"].(string)
	h.want(owner, "PATCH", "/api/v1/observability/sinks/"+sinkID, map[string]any{"name": "renamed", "enabled": true}, map[string]string{"If-Match": `"` + etag + `"`}, 200)
	enqueueFixture(t, h.Pool, "requests", nil, "team-chat", "success", map[string]any{"id": "after-patch"})
	w := exportWorker(t, h.Pool, h.Server.Installation, h.Ring2(), loopbackPolicy())
	if err := w.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	awaitEnvelope(t, receiver, 1)

	detail := h.want(owner, "GET", "/api/v1/observability/sinks/"+sinkID, nil, nil, 200)
	if detail["name"] != "renamed" {
		t.Fatalf("patch lost name: %v", detail)
	}
	etag = detail["etag"].(string)
	h.want(owner, "PATCH", "/api/v1/observability/sinks/"+sinkID, map[string]any{"filter": map[string]any{"route": "team-chat"}}, map[string]string{"If-Match": `"` + etag + `"`}, 200)
	detail = h.want(owner, "GET", "/api/v1/observability/sinks/"+sinkID, nil, nil, 200)
	etag = detail["etag"].(string)
	h.want(owner, "PATCH", "/api/v1/observability/sinks/"+sinkID, map[string]any{"name": "still-renamed"}, map[string]string{"If-Match": `"` + etag + `"`}, 200)
	detail = h.want(owner, "GET", "/api/v1/observability/sinks/"+sinkID, nil, nil, 200)
	if detail["filter"].(map[string]any)["route"] != "team-chat" {
		t.Fatalf("partial route filter lost on unrelated patch: %v", detail["filter"])
	}
	if pending := pendingCount(t, h.Pool, sinkID); pending != 0 {
		t.Fatalf("record pending after successful pass: %d", pending)
	}
	enqueueFixture(t, h.Pool, "requests", nil, "other-route", "success", map[string]any{"id": "filtered-out"})
	enqueueFixture(t, h.Pool, "requests", nil, "team-chat", "success", map[string]any{"id": "kept"})
	if err := w.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	envs := awaitEnvelope(t, receiver, 2)
	for _, env := range envs {
		if env["data"].(map[string]any)["id"] == "filtered-out" {
			t.Fatal("route filter did not exclude the foreign route")
		}
	}
	etag = h.want(owner, "GET", "/api/v1/observability/sinks/"+sinkID, nil, nil, 200)["etag"].(string)
	h.want(owner, "PATCH", "/api/v1/observability/sinks/"+sinkID, map[string]any{"filter": map[string]any{}}, map[string]string{"If-Match": `"` + etag + `"`}, 200)
	if f := h.want(owner, "GET", "/api/v1/observability/sinks/"+sinkID, nil, nil, 200)["filter"]; len(f.(map[string]any)) != 0 {
		t.Fatalf("empty object filter not cleared: %v", f)
	}
	etag = h.want(owner, "GET", "/api/v1/observability/sinks/"+sinkID, nil, nil, 200)["etag"].(string)
	h.want(owner, "PATCH", "/api/v1/observability/sinks/"+sinkID, map[string]any{"filter": map[string]any{"route": "team-chat"}}, map[string]string{"If-Match": `"` + etag + `"`}, 200)
	etag = h.want(owner, "GET", "/api/v1/observability/sinks/"+sinkID, nil, nil, 200)["etag"].(string)
	h.want(owner, "PATCH", "/api/v1/observability/sinks/"+sinkID, map[string]any{"filter": nil}, map[string]string{"If-Match": `"` + etag + `"`}, 200)
	if f := h.want(owner, "GET", "/api/v1/observability/sinks/"+sinkID, nil, nil, 200)["filter"].(map[string]any); f["route"] != nil {
		t.Fatalf("null filter did not clear: %v", f)
	}
	enqueueFixture(t, h.Pool, "requests", nil, "other-route", "success", map[string]any{"id": "post-clear"})
	if err := w.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, env := range awaitEnvelope(t, receiver, 3) {
		if env["data"].(map[string]any)["id"] == "post-clear" {
			found = true
		}
	}
	if !found {
		t.Fatal("cleared filter suppressed deliveries")
	}
}

func TestExportSinkValidationRejectsBadFilterAndUUIDs(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	t.Cleanup(server.Close)
	h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "bad-filter", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
		"filter": map[string]any{"unknown_key": "x"},
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 422)
	h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "bad-project", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
		"project_id": "not-a-uuid",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 422)
	h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "bad-filter-project", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
		"filter": map[string]any{"project": "nope"},
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 422)
	h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "bad-filter-project-2", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
		"filter": map[string]any{"project": uuid.NewString()},
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 422)
	h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "bad-filter-route", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
		"filter": map[string]any{"route": "no-such-route"},
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 422)
}

func TestExportSinkCredentialClear(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	t.Cleanup(server.Close)
	sink := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "cred", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
		"credential": map[string]any{"signing_secret": "0123456789abcdef0123456789abcdef"},
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	if sink["credential_configured"] != true {
		t.Fatal("credential not configured")
	}
	sinkID := sink["export_sink_id"].(string)
	var credentialID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT credential_id::text FROM olp.export_sinks WHERE id=$1`, sinkID).Scan(&credentialID); err != nil {
		t.Fatal(err)
	}
	etag := h.want(owner, "GET", "/api/v1/observability/sinks/"+sinkID, nil, nil, 200)["etag"].(string)
	h.want(owner, "PATCH", "/api/v1/observability/sinks/"+sinkID, map[string]any{"credential": nil}, map[string]string{"If-Match": `"` + etag + `"`}, 200)
	var leftover *string
	if err := h.Pool.QueryRow(t.Context(), `SELECT credential_id::text FROM olp.export_sinks WHERE id=$1`, sinkID).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover != nil {
		t.Fatal("credential_id remained after clear")
	}
	var secrets int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.secrets WHERE id=$1`, credentialID).Scan(&secrets); err != nil || secrets != 0 {
		t.Fatal("old sealed credential leaked")
	}
}

func TestExportMaintenanceRecordsGapDuringStall(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	block := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(block) }) })
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-block
		w.WriteHeader(204)
	}))
	t.Cleanup(blocked.Close)
	fast, fastServer := newExportReceiver(t, "", 204)
	_ = fast
	stalled := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "stalled", "type": "https", "destination": blocked.URL, "streams": []string{"requests"}, "format": "json",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	gap := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "gap-sink", "type": "https", "destination": fastServer.URL, "streams": []string{"requests"}, "format": "json",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	old := time.Now().Add(-31 * 24 * time.Hour)
	source := uuid.NewString()
	if _, err := h.Pool.Exec(t.Context(), `SELECT olp.enqueue_export('requests',$1::uuid,NULL,'team-chat','success',$2,$3::jsonb)`, source, old, `{"id":"expired"}`); err != nil {
		t.Fatal(err)
	}
	gapID := gap["export_sink_id"].(string)
	stalledID := stalled["export_sink_id"].(string)
	for _, id := range []string{gapID, stalledID} {
		var pending int64
		if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.export_pending WHERE sink_id=$1`, id).Scan(&pending); err != nil || pending != 1 {
			t.Fatalf("fixture not pending for %s: %d %v", id, pending, err)
		}
	}
	w := exportWorker(t, h.Pool, h.Server.Installation, h.Ring2(), loopbackPolicy())
	w.PassBudget = 350 * time.Millisecond
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("pass with a stalled sink must stay healthy: %v", err)
	}
	var gaps int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT coalesce(sum(record_count),0) FROM olp.export_gaps WHERE sink_id=$1`, gapID).Scan(&gaps); err != nil {
		t.Fatal(err)
	}
	if gaps != 1 {
		t.Fatalf("expired record produced %d gaps, want 1 while a stalled sink exhausted the budget", gaps)
	}
	if pending := pendingCount(t, h.Pool, gapID); pending != 0 {
		t.Fatal("expired record stayed pending instead of becoming a gap")
	}
	releaseOnce.Do(func() { close(block) })
}

func TestExportWorkerDatabaseFailureMarksUnhealthy(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	receiver, server := newExportReceiver(t, "", 204)
	h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "db-fail", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	pool, err := pgxpool.New(t.Context(), h.DBURL)
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	w := exportWorker(t, pool, h.Server.Installation, h.Ring2(), loopbackPolicy())
	if err := w.Pass(t.Context()); err == nil {
		t.Fatal("pass on a closed pool must report unhealthy")
	}
	if got := receiver.received(); len(got) != 0 {
		t.Fatalf("dead worker delivered %d records", len(got))
	}
}

func TestCaptureGatewayDeliversUnaryAndStreamToOwnedSink(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	h.Gateway.Sink = &gateway.PersistingSink{Persist: func(ctx context.Context, event *usage.Event, payload []byte) error {
		_, err := usage.PersistEvent(ctx, h.Pool, event, payload)
		return err
	}}
	owner, _, slug, secret := provisionOpenAIWith(t, h, fixture.URL,
		[]any{
			map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"},
			map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"},
		},
		[]string{"generation"}, nil)

	detail := h.want(owner, "GET", "/api/v1/observability/capture", nil, nil, 200)
	if enabled := detail["enabled"]; enabled != false {
		t.Fatalf("capture enabled before owner opt-in: %v", enabled)
	}
	h.want(owner, "PATCH", "/api/v1/observability/capture", map[string]any{"enabled": true}, map[string]string{"If-Match": `"` + detail["etag"].(string) + `"`}, 200)
	receiver, server := newExportReceiver(t, "", 204)
	exportSink := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "capture-target", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.want(owner, "POST", "/api/v1/observability/capture-policies", map[string]any{
		"sink": exportSink["export_sink_id"], "sample_ratio": "1", "include": []string{"input", "output", "tool_calls"},
		"max_bytes": 1048576, "route_slug": slug,
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)

	manager := &export.Manager{Pool: h.Pool, Keys: h.Ring2(), Installation: h.Server.Installation, Sender: export.NewSender(loopbackPolicy()), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h.Gateway.Capture = manager
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { manager.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	time.Sleep(300 * time.Millisecond)

	status, _, _ := h.gateway("POST", "/v1/chat/completions", secret,
		map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "capture-sentinel-input"}}})
	if status != http.StatusOK {
		t.Fatalf("unary request = %d", status)
	}
	status, raw, _ := h.gatewayRaw("POST", "/v1/chat/completions", secret,
		strings.NewReader(`{"model":"`+slug+`","messages":[{"role":"user","content":"capture-sentinel-stream"}],"stream":true}`),
		map[string]string{"Content-Type": "application/json"})
	if status != http.StatusOK || !strings.Contains(string(raw), "OK") {
		t.Fatalf("streamed request = %d %s", status, raw)
	}
	envs := awaitEnvelope(t, receiver, 2)
	var unary, streamed map[string]any
	for _, env := range envs {
		data, _ := env["data"].(map[string]any)
		if data["mode"] == "streaming" {
			streamed = data
		} else if data["mode"] == "unary" || data["mode"] == nil {
			unary = data
		}
	}
	if unary == nil || streamed == nil {
		t.Fatalf("expected unary and streamed capture records, got %v", envs)
	}
	if encoded, _ := json.Marshal(unary["input"]); !strings.Contains(string(encoded), "capture-sentinel-input") {
		t.Fatalf("unary input not captured: %v", unary["input"])
	}
	if unary["output"] != "OK" {
		t.Fatalf("unary output = %v", unary["output"])
	}
	if encoded, _ := json.Marshal(streamed["input"]); !strings.Contains(string(encoded), "capture-sentinel-stream") {
		t.Fatalf("streamed input not captured: %v", streamed["input"])
	}
	if streamed["output"] != "OK" {
		t.Fatalf("streamed output = %v", streamed["output"])
	}
	var captured int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.requests WHERE payload_captured`).Scan(&captured); err != nil {
		t.Fatal(err)
	}
	if captured != 2 {
		t.Fatalf("payload_captured persisted on %d requests, want 2", captured)
	}
	var leaked int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.audit a WHERE a.action IN ('capture.delivered','capture.failed','capture.dropped') AND to_json(a)::text LIKE '%capture-sentinel%'`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatal("capture outcome audits contain captured content")
	}
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.audit WHERE action='capture.delivered'`).Scan(&leaked); err != nil || leaked < 2 {
		t.Fatalf("delivered outcomes not audited: %d %v", leaked, err)
	}
	var events int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.requests r WHERE to_json(r)::text LIKE '%capture-sentinel-input%'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatal("request metadata rows contain captured content")
	}
	var pendingLeak int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.export_records WHERE payload::text LIKE '%capture-sentinel%'`).Scan(&pendingLeak); err != nil {
		t.Fatal(err)
	}
	if pendingLeak != 0 {
		t.Fatal("durable export rows contain captured content")
	}
}

func TestExportPassPropagatesDBFailureAcrossDeadline(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	t.Cleanup(server.Close)
	for _, name := range []string{"p1", "p2"} {
		h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
			"name": name, "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
		}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	}
	w := exportWorker(t, h.Pool, h.Server.Installation, h.Ring2(), loopbackPolicy())
	w.PassBudget = 50 * time.Millisecond
	boom := errors.New("claim failed")
	var calls atomic.Int64
	w.DrainPair = func(ctx context.Context, sink, stream string) (bool, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return false, boom
		}
		return false, nil
	}
	err := w.Pass(t.Context())
	if !errors.Is(err, boom) {
		t.Fatalf("pass deadline lost the earlier DB failure: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("deadline pass attempted %d pairs", calls.Load())
	}
}

func TestCapturePolicyProjectManagerScope(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fixtureRoute(t, h, "team-chat")
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Capture scope"}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	projectID := project["id"].(string)
	other := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Foreign scope"}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	otherID := other["id"].(string)
	manager := h.invite(owner, "capture-manager@example.com", "developer")
	profile := h.want(manager, "GET", "/api/v1/profile", nil, nil, 200)
	managerID := profile["id"].(string)
	h.want(owner, "PATCH", "/api/v1/users/"+managerID, map[string]any{"access_scope": "assigned"}, etagHeader(profile), 200)
	addMember(h, owner, projectID, managerID, "manager")
	manager = login(h, "capture-manager@example.com")
	viewer := h.invite(owner, "capture-viewer@example.com", "viewer")

	receiver, server := newExportReceiver(t, "", 204)
	_ = receiver
	sink := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "pm-sink", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	sinkID := sink["export_sink_id"].(string)
	policyBody := func(project any) map[string]any {
		return map[string]any{"sink": sinkID, "sample_ratio": "1", "include": []string{"output"},
			"max_bytes": 1048576, "project_id": project, "route_slug": "team-chat"}
	}
	h.want(manager, "POST", "/api/v1/observability/capture-policies", policyBody(projectID),
		map[string]string{"Idempotency-Key": uuid.NewString()}, 422)
	h.want(owner, "PATCH", "/api/v1/observability/capture", map[string]any{"enabled": true},
		etagHeader(h.want(owner, "GET", "/api/v1/observability/capture", nil, nil, 200)), 200)
	h.want(manager, "PATCH", "/api/v1/observability/capture", map[string]any{"enabled": true},
		etagHeader(h.want(owner, "GET", "/api/v1/observability/capture", nil, nil, 200)), 403)

	created := h.want(manager, "POST", "/api/v1/observability/capture-policies", policyBody(projectID),
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	policyID := created["id"].(string)
	if status, _, _ := h.request(manager, "POST", "/api/v1/observability/capture-policies", policyBody(nil),
		map[string]string{"Idempotency-Key": uuid.NewString()}); status != 403 {
		t.Fatalf("manager installation policy = %d, want 403", status)
	}
	if status, _, _ := h.request(manager, "POST", "/api/v1/observability/capture-policies", policyBody(otherID),
		map[string]string{"Idempotency-Key": uuid.NewString()}); status == 201 {
		t.Fatal("manager foreign-project policy accepted")
	}
	h.want(viewer, "POST", "/api/v1/observability/capture-policies", policyBody(projectID),
		map[string]string{"Idempotency-Key": uuid.NewString()}, 403)

	updated := h.want(manager, "PATCH", "/api/v1/observability/capture-policies/"+policyID,
		map[string]any{"sample_ratio": "0.5"}, etagHeader(created), 200)
	if updated["sample_ratio"] != "0.5" {
		t.Fatalf("policy update %v", updated["sample_ratio"])
	}
	h.want(manager, "DELETE", "/api/v1/observability/capture-policies/"+policyID,
		nil, etagHeader(updated), 204)

	narrow := h.want(owner, "POST", "/api/v1/observability/capture-policies", policyBody(projectID),
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	narrowID := narrow["id"].(string)
	h.want(owner, "PATCH", "/api/v1/observability/capture", map[string]any{"enabled": false},
		etagHeader(h.want(owner, "GET", "/api/v1/observability/capture", nil, nil, 200)), 200)
	sinkDisabled := h.want(owner, "PATCH", "/api/v1/observability/sinks/"+sinkID,
		map[string]any{"enabled": false}, etagHeader(sink), 200)
	if status, _, _ := h.request(owner, "POST", "/api/v1/observability/capture-policies", policyBody(projectID),
		map[string]string{"Idempotency-Key": uuid.NewString()}); status != 422 {
		t.Fatalf("create while disabled = %d, want 422", status)
	}
	disabled := h.want(owner, "PATCH", "/api/v1/observability/capture-policies/"+narrowID,
		map[string]any{"enabled": false}, etagHeader(narrow), 200)
	if disabled["enabled"] != false {
		t.Fatalf("pure disable while master off = %v", disabled["enabled"])
	}
	if status, _, _ := h.request(owner, "PATCH", "/api/v1/observability/capture-policies/"+narrowID,
		map[string]any{"enabled": true}, etagHeader(disabled)); status != 422 {
		t.Fatalf("enable while master off = %d, want 422", status)
	}
	if status, _, _ := h.request(owner, "PATCH", "/api/v1/observability/capture-policies/"+narrowID,
		map[string]any{"sample_ratio": "0.5"}, etagHeader(disabled)); status != 422 {
		t.Fatalf("widen while master off = %d, want 422", status)
	}
	if status, _, _ := h.request(owner, "PATCH", "/api/v1/observability/capture-policies/"+narrowID,
		map[string]any{"enabled": false}, nil); status != 428 {
		t.Fatalf("pure disable without ETag = %d, want 428", status)
	}
	h.want(owner, "DELETE", "/api/v1/observability/capture-policies/"+narrowID,
		nil, etagHeader(disabled), 204)
	h.want(owner, "PATCH", "/api/v1/observability/sinks/"+sinkID,
		map[string]any{"enabled": true}, etagHeader(sinkDisabled), 200)
}

func TestRuntimeInstallHeartbeatRecordsFailureAndRecovery(t *testing.T) {
	h := newAccessHarness(t)
	h.owner()
	status := func() (desired, installed int64, failed bool) {
		var d, i int64
		var f bool
		err := h.Pool.QueryRow(t.Context(),
			`SELECT desired_generation,installed_generation,failed FROM olp.runtime_install_status WHERE gateway_instance=$1`,
			h.Runtime.InstanceID).Scan(&d, &i, &f)
		if err != nil {
			t.Fatal(err)
		}
		return d, i, f
	}
	if h.Runtime.InstanceID == "" {
		t.Fatal("manager has no instance ID")
	}
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatalf("baseline refresh: %v", err)
	}
	d, i, f := status()
	if f || d != i {
		t.Fatalf("baseline status %d %d %v", d, i, f)
	}
	var badSequence int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT release_sequence+1 FROM olp.installation WHERE singleton`).Scan(&badSequence); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(),
		`INSERT INTO olp.runtime_releases(id,sequence,sha256,snapshot,created_by)
		 VALUES($1,$2,'deadbeef','{"routes":{"bad":{}}}'::json,(SELECT id FROM olp.users LIMIT 1))`, uuid.New(), badSequence); err != nil {
		t.Fatal(err)
	}
	if err := h.Runtime.Refresh(t.Context()); err == nil {
		t.Fatal("invalid release installed")
	}
	d, i, f = status()
	if d != badSequence || i == badSequence || !f {
		t.Fatalf("failed install status %d,%d,%v", d, i, f)
	}
	var ownerID string
	if err := h.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.users ORDER BY created_at LIMIT 1").Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `DELETE FROM olp.runtime_releases WHERE sequence=$1`, badSequence); err != nil {
		t.Fatal(err)
	}
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	published, err := runtime.Publish(t.Context(), tx, ownerID)
	if err != nil {
		tx.Rollback(t.Context())
		t.Fatalf("publish replacement release: %v", err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	d, i, f = status()
	if d != published.Sequence || i != published.Sequence || f {
		t.Fatalf("recovery status %d,%d,%v want %d installed", d, i, f, published.Sequence)
	}
}

func TestCaptureSinksListIsMetadataOnly(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	receiver, server := newExportReceiver(t, "", 204)
	_ = receiver
	sink := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "capture-option", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "disabled-option", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json", "enabled": false,
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	member := h.invite(owner, "sink-picker@example.com", "developer")
	for _, caller := range []*browser{owner, member} {
		listed := h.want(caller, "GET", "/api/v1/observability/capture-sinks", nil, nil, 200)
		items, ok := listed["items"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("capture sinks %v", listed)
		}
		item := items[0].(map[string]any)
		if item["export_sink_id"] != sink["export_sink_id"] || item["name"] != "capture-option" || item["type"] != "https" {
			t.Fatalf("sink option %v", item)
		}
		if len(item) != 3 {
			t.Fatalf("sink option carries destination/credentials: %v", item)
		}
	}
}
