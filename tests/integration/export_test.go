//go:build integration

package integration_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/export"
	"github.com/tyk-swe/olp/internal/gateway"
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

func (h *accessHarness) Ring2() *secrets.KeyRing {
	ring, err := secrets.ParseRing([]byte(h.Ring))
	if err != nil {
		h.t.Fatal(err)
	}
	return ring
}

// The claim lease expires; the retried attempt posts the same immutable
// event id, so the receiver can deduplicate at-least-once delivery.

// A record committed after the cursor advanced is still delivered: no
// cursor position can skip a committed pending entry.

// An audit row for the project is exported; other-project and installation
// rows are not.

func fixtureRoute(t *testing.T, h *accessHarness, slug string) {
	t.Helper()
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.routes(id,slug,created_by,latest_revision,latest_revision_id,etag) VALUES(gen_random_uuid(),$1,(SELECT id FROM olp.users LIMIT 1),1,gen_random_uuid(),gen_random_uuid())`, slug); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureStalePolicyDropsQueuedRecord(t *testing.T) {
	t.Run("unredacted", func(t *testing.T) { testCaptureStalePolicyDropsQueuedRecord(t, false) })
	t.Run("redactor-refresh", func(t *testing.T) { testCaptureStalePolicyDropsQueuedRecord(t, true) })
}

func testCaptureStalePolicyDropsQueuedRecord(t *testing.T, redacted bool) {
	h := newAccessHarness(t)
	owner := h.owner()
	fixtureRoute(t, h, "team-chat")
	projectID := ""
	redactors := []string{}
	if redacted {
		projectID = createProject(h, owner, "Capture stale redactors")
		if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.routes SET project_id=$1 WHERE slug='team-chat'`, projectID); err != nil {
			t.Fatal(err)
		}
		captureDefinition(t, h, owner, projectID, "private", "builtin.keywords", map[string]any{"words": []string{"sentinel"}}, true)
		redactors = []string{"private"}
	}
	h.want(owner, "PATCH", "/api/v1/observability/capture", map[string]any{"enabled": true}, etagHeader(h.want(owner, "GET", "/api/v1/observability/capture", nil, nil, 200)), 200)
	receiver, server := newExportReceiver(t, "", 204)
	sink := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "capture-stale", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	policy := h.want(owner, "POST", "/api/v1/observability/capture-policies", map[string]any{
		"sink": sink["export_sink_id"], "sample_ratio": "1", "include": []string{"output"}, "max_bytes": 1048576, "route_slug": "team-chat", "redact": redactors,
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
	collector := manager.Resolve(projectID, "team-chat", "", "", uuid.NewString())
	if collector == nil {
		t.Fatal("not sampled")
	}
	collector.CaptureText("output", "fresh-sentinel")
	if !collector.Finish(export.CaptureIdentity{Route: "team-chat", ProjectID: projectID, Outcome: "success"}, time.Now()) {
		t.Fatal("not queued")
	}
	time.Sleep(100 * time.Millisecond)
	// Updating the policy bumps its etag while the first record is mid-send;
	// the second queued record resolves against the old etag and is dropped.
	policyID := policy["id"].(string)
	update := map[string]any{"sample_ratio": "0.5"}
	if redacted {
		update = map[string]any{"redact": redactors}
	}
	h.want(owner, "PATCH", "/api/v1/observability/capture-policies/"+policyID, update, map[string]string{"If-Match": `"` + policy["etag"].(string) + `"`}, 200)
	staleRequest := uuid.NewString()
	stale := manager.Resolve(projectID, "team-chat", "", "", staleRequest)
	if stale == nil {
		t.Fatal("second request not sampled")
	}
	stale.CaptureText("output", "stale-sentinel")
	if !stale.Finish(export.CaptureIdentity{Route: "team-chat", ProjectID: projectID, Outcome: "success"}, time.Now()) {
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
		if env["source_id"] == staleRequest {
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
