//go:build integration

package integration_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway"

	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/sinks"
	"github.com/tyk-swe/olp/internal/usage"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestManagedEvidenceDeliveryRechecksAuthorityAndRetention(t *testing.T) {
	h := newAccessHarness(t)
	_, _, limiter := limSetup(t, "managed-evidence")
	h.Gateway.Traces = &gateway.TraceDeps{Pool: h.Pool, Limiter: limiter, Auth: h.AuthKey}
	owner := h.owner()
	project := createProject(h, owner, "Managed evidence")
	member := h.invite(owner, "evidence-manager@example.test", "developer")
	memberID := mcpExecString(t, h.want(member, http.MethodGet, "/api/v1/profile", nil, nil, http.StatusOK), "id")
	memberRecord := h.want(owner, http.MethodGet, "/api/v1/users/"+memberID, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPatch, "/api/v1/users/"+memberID, map[string]any{"access_scope": "assigned"}, etagHeader(memberRecord), http.StatusOK)
	addMember(h, owner, project, memberID, "manager")
	h.want(member, http.MethodPost, "/api/v1/sessions", map[string]any{"email": "evidence-manager@example.test", "password": accessPassword}, nil, http.StatusCreated)
	received := make(chan []byte, 8)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		received <- payload
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()
	foreignReceived := make(chan struct{}, 1)
	foreignReceiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreignReceived <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer foreignReceiver.Close()
	foreignProject := createProject(h, owner, "Other evidence")
	h.want(owner, http.MethodPost, "/api/v1/sinks", map[string]any{"name": "Other project evidence", "type": "https", "project_id": foreignProject, "destination": foreignReceiver.URL, "streams": []string{"trace_spans"}, "enabled": true}, idem(uuid.NewString()), http.StatusCreated)
	h.want(owner, http.MethodPost, "/api/v1/sinks", map[string]any{"name": "Unscoped evidence", "type": "https", "destination": receiver.URL, "streams": []string{"runs"}, "enabled": true}, idem(uuid.NewString()), http.StatusUnprocessableEntity)
	h.want(member, http.MethodPost, "/api/v1/sinks", map[string]any{"name": "Project evidence", "type": "https", "project_id": project, "destination": receiver.URL, "streams": []string{"runs", "trace_spans"}, "enabled": true}, idem(uuid.NewString()), http.StatusCreated)
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Evidence writer", "project_id": project, "scopes": []string{"traces_write"}}, idem(uuid.NewString()), http.StatusCreated)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	writer := mcpExecString(t, key, "secret")
	keyID := mcpExecString(t, key, "id")
	worker := &sinks.Worker{Pool: h.Pool, Egress: &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}}
	span := func(id byte) *tracepb.Span {
		return &tracepb.Span{TraceId: bytes.Repeat([]byte{id}, 16), SpanId: bytes.Repeat([]byte{id}, 8), Name: "private-span-content", Attributes: []*commonpb.KeyValue{traceText("gen_ai.prompt", "private-prompt-content"), traceText("olp.evidence_origin", "gateway_observed")}}
	}
	if status, body := postTrace(h, writer, tracePayload(t, traceBatch(span(1)))); status != http.StatusOK {
		t.Fatalf("first trace upload: %d %s", status, body)
	}
	if _, err := worker.Deliver(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-received:
		var event struct {
			Stream string `json:"stream"`
			Data   struct {
				ProjectID string `json:"project_id"`
				Origin    string `json:"evidence_origin"`
			} `json:"data"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatal(err)
		}
		if event.Stream != "trace_spans" || event.Data.ProjectID != project || event.Data.Origin != "client_asserted" {
			t.Fatalf("unexpected trace evidence metadata: %+v", event)
		}
		if bytes.Contains(payload, []byte("private-")) || bytes.Contains(payload, []byte(writer)) {
			t.Fatal("trace evidence included private content or a credential")
		}
	default:
		t.Fatal("authorized trace evidence was not delivered")
	}
	select {
	case <-foreignReceived:
		t.Fatal("trace evidence was delivered to another project")
	default:
	}
	if status, body := postTrace(h, writer, tracePayload(t, traceBatch(span(2)))); status != http.StatusOK {
		t.Fatalf("second trace upload: %d %s", status, body)
	}
	h.want(owner, http.MethodDelete, "/api/v1/projects/"+project+"/members/"+memberID, nil, projectEtag(h, owner, project), http.StatusNoContent)
	if _, err := worker.Deliver(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-received:
		t.Fatalf("evidence delivered after project access was revoked: %d bytes", len(payload))
	default:
	}
	addMember(h, owner, project, memberID, "manager")
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.settings(key,value,etag,updated_by) SELECT 'retention.requests_days','1',gen_random_uuid(),created_by FROM olp.api_keys WHERE id=$1 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, keyID); err != nil {
		t.Fatal(err)
	}
	if status, body := postTrace(h, writer, tracePayload(t, traceBatch(span(3)))); status != http.StatusOK {
		t.Fatalf("expiring trace upload: %d %s", status, body)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.trace_spans SET received_at=now()-interval '2 days' WHERE api_key_id=$1 AND trace_id=$2`, keyID, bytes.Repeat([]byte{3}, 16)); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Deliver(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-received:
		t.Fatalf("evidence delivered after its retention period: %d bytes", len(payload))
	default:
	}
	if status, body := postTrace(h, writer, tracePayload(t, traceBatch(span(4)))); status != http.StatusOK {
		t.Fatalf("removed trace upload: %d %s", status, body)
	}
	if _, err := h.Pool.Exec(t.Context(), `DELETE FROM olp.trace_spans WHERE api_key_id=$1 AND trace_id=$2`, keyID, bytes.Repeat([]byte{4}, 16)); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Deliver(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-received:
		t.Fatalf("evidence delivered after its source was removed: %d bytes", len(payload))
	default:
	}
}

func TestExportSinksQueueScopedFactsAndRetrySignedStableEvents(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Export project")
	createCatalogRoute(h, owner, project, "sink-route")
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Export key", "project_id": project}, idem(uuid.NewString()), 201)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	const secret = "private-sink-signing-marker"
	type seen struct {
		path, id, signature string
		body                []byte
	}
	var mu sync.Mutex
	var received []seen
	var fail atomic.Bool
	fail.Store(true)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 65537))
		mu.Lock()
		received = append(received, seen{r.URL.Path, r.Header.Get("Idempotency-Key"), r.Header.Get("X-OLP-Signature"), body})
		mu.Unlock()
		if r.URL.Path == "/global" && fail.Load() {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer receiver.Close()
	global := h.want(owner, "POST", "/api/v1/sinks", map[string]any{"name": "Global facts", "type": "https", "destination": receiver.URL + "/global", "streams": []string{"requests", "attempts", "guardrail_decisions"}, "enabled": true, "credential": secret}, idem(uuid.NewString()), 201)
	validateManagementResponse(t, "POST", "/api/v1/sinks", 201, global)
	projectSink := h.want(owner, "POST", "/api/v1/sinks", map[string]any{"name": "Project facts", "type": "https", "project_id": project, "destination": receiver.URL + "/project", "streams": []string{"requests"}, "enabled": true}, idem(uuid.NewString()), 201)
	foreign := createProject(h, owner, "Unrelated exports")
	h.want(owner, "POST", "/api/v1/sinks", map[string]any{"name": "Foreign facts", "type": "https", "project_id": foreign, "destination": receiver.URL + "/foreign", "streams": []string{"requests"}, "enabled": true}, idem(uuid.NewString()), 201)
	status := 400
	errorClass := "content_policy_blocked"
	now := time.Now().UTC()
	event := &usage.Event{Version: usage.WireVersion, EventID: uuid.NewString(), RequestID: uuid.NewString(), RuntimeGenerationID: h.Runtime.Release().Snapshot.Generation.ID, APIKeyID: key["id"].(string), RouteSlug: "sink-route", Operation: "generation", Surface: "openai", RequestStartedAt: now.Add(-time.Millisecond), RequestCompletedAt: now, ObservedAt: now, StatusCode: &status, ErrorClass: &errorClass, LatencyMS: 1, Attribution: map[string]string{"private": "excluded-attribution-marker"}, PolicyDecisions: []contentpolicy.Decision{{RuleID: "filter", Phase: "input", Action: "block", Outcome: "blocked"}}}
	payload, _ := json.Marshal(event)
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = usage.PersistEventTx(t.Context(), tx, event, payload); err != nil {
		t.Fatal(err)
	}
	var visible int
	if err = h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.managed_export_deliveries").Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("uncommitted export escaped: %d %v", visible, err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if result, err := usage.PersistEvent(t.Context(), h.Pool, event, payload); err != nil || result.Outcome != usage.PersistOutcomeDuplicate {
		t.Fatalf("duplicate source: %v %v", result, err)
	}
	var count int
	if err = h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.managed_export_deliveries").Scan(&count); err != nil || count != 3 {
		t.Fatalf("fanout/duplicate isolation: %d %v", count, err)
	}
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	installation, err := database.Installation(t.Context(), h.Pool)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing([]byte(h.Ring))
	if err != nil {
		t.Fatal(err)
	}
	worker := &sinks.Worker{Pool: h.Pool, Keys: ring, Installation: installation, Egress: policy}
	if progress, err := worker.Deliver(t.Context()); err != nil || !progress {
		t.Fatalf("first delivery: %v %v", progress, err)
	}
	fail.Store(false)
	if _, err = h.Pool.Exec(t.Context(), "UPDATE olp.managed_export_deliveries SET next_attempt_at=now() WHERE status='pending'"); err != nil {
		t.Fatal(err)
	}
	if _, err = worker.Deliver(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	seenEvents := append([]seen(nil), received...)
	mu.Unlock()
	byID := map[string]seen{}
	duplicates := 0
	for _, got := range seenEvents {
		if got.path == "/foreign" {
			t.Fatal("project boundary leaked")
		}
		if strings.Contains(string(got.body), secret) || strings.Contains(string(got.body), "excluded-attribution-marker") || strings.Contains(string(got.body), key["secret"].(string)) {
			t.Fatal("export carried secret or arbitrary labels")
		}
		if got.path == "/global" {
			sum := hmac.New(sha256.New, []byte(secret))
			_, _ = sum.Write(got.body)
			if got.signature != "sha256="+hex.EncodeToString(sum.Sum(nil)) {
				t.Fatal("signature does not bind delivered bytes")
			}
		}
		identity := got.path + got.id
		if previous, ok := byID[identity]; ok {
			duplicates++
			if string(previous.body) != string(got.body) {
				t.Fatal("retry changed event bytes")
			}
		}
		byID[identity] = got
	}
	if len(byID) != 3 || duplicates != 2 {
		t.Fatalf("stable delivery identities: %d duplicates %d", len(byID), duplicates)
	}
	var failed, delivered int
	if err = h.Pool.QueryRow(t.Context(), "SELECT failed_total,delivered_total FROM olp.managed_export_sinks WHERE id=$1", global["id"]).Scan(&failed, &delivered); err != nil || failed != 2 || delivered != 2 {
		t.Fatalf("delivery counters %d %d %v", failed, delivered, err)
	}
	// Attempt inserts and later pricing corrections are separate immutable export events.
	var providerID string
	if err = h.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.providers WHERE state='active' LIMIT 1").Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	limSeedFact(t, h.Pool, providerID, key["id"].(string), now, nil)
	if _, err = h.Pool.Exec(t.Context(), "UPDATE olp.attempt_usage_facts SET estimated_cost='1.234567890123',unpriced=false WHERE api_key_id=$1", key["id"]); err != nil {
		t.Fatal(err)
	}
	if _, err = worker.Deliver(t.Context()); err != nil {
		t.Fatal(err)
	}
	var attempts int
	if err = h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.managed_export_deliveries WHERE stream='attempts' AND status='delivered'").Scan(&attempts); err != nil || attempts != 2 {
		t.Fatalf("attempt/correction exports %d %v", attempts, err)
	}
	// Expired pending payloads record gaps independently of fact retention.
	if _, err = h.Pool.Exec(t.Context(), "INSERT INTO olp.managed_export_deliveries(sink_id,event_id,stream,payload,expires_at) VALUES($1,$2,'requests','{}',now()-interval '1 second')", global["id"], uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err = worker.Deliver(t.Context()); err != nil {
		t.Fatal(err)
	}
	var expired int
	if err = h.Pool.QueryRow(t.Context(), "SELECT expired_total FROM olp.managed_export_sinks WHERE id=$1", global["id"]).Scan(&expired); err != nil || expired != 1 {
		t.Fatalf("expiry gap %d %v", expired, err)
	}
	path := "/api/v1/sinks/" + global["id"].(string)
	h.want(owner, "PUT", path, map[string]any{"name": "Changed", "destination": receiver.URL, "streams": []string{"requests"}, "enabled": false}, idem(uuid.NewString()), 428)
	retire := map[string]string{"If-Match": `"` + global["etag"].(string) + `"`, "Idempotency-Key": uuid.NewString()}
	h.want(owner, "DELETE", path, nil, retire, 204)
	h.want(owner, "DELETE", path, nil, retire, 204)
	var sealedCount int
	if err = h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.secrets WHERE purpose=$1", secrets.SinkCredential).Scan(&sealedCount); err != nil || sealedCount != 0 {
		t.Fatalf("retired signing secret remains: %d %v", sealedCount, err)
	}
	auditSink := h.want(owner, "POST", "/api/v1/sinks", map[string]any{"name": "Audit facts", "type": "https", "destination": receiver.URL + "/audit", "streams": []string{"audit"}, "enabled": true}, idem(uuid.NewString()), 201)
	if _, err = h.Pool.Exec(t.Context(), "INSERT INTO olp.audit(id,actor_user_id,action,resource_type,resource_id,outcome,source_ip,user_agent_family) VALUES($1,$2,'user.update','user','excluded-resource-marker','success','127.0.0.1','excluded-agent-marker')", uuid.NewString(), h.want(owner, "GET", "/api/v1/sessions/current", nil, nil, 200)["user"].(map[string]any)["id"]); err != nil {
		t.Fatal(err)
	}
	if _, err = worker.Deliver(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	for _, got := range received {
		if got.path == "/audit" && (strings.Contains(string(got.body), "excluded-resource-marker") || strings.Contains(string(got.body), "excluded-agent-marker") || strings.Contains(string(got.body), "127.0.0.1")) {
			t.Error("audit export carried identity/network metadata")
		}
	}
	mu.Unlock()
	var auditCount int
	if err = h.Pool.QueryRow(t.Context(), "SELECT delivered_total FROM olp.managed_export_sinks WHERE id=$1", auditSink["id"]).Scan(&auditCount); err != nil || auditCount < 2 {
		t.Fatalf("audit delivery %d %v", auditCount, err)
	}
	_ = projectSink
}
