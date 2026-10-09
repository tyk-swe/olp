//go:build integration

package integration_test

import (
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
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/sinks"
	"github.com/tyk-swe/olp/internal/usage"
)

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
	if err = h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.export_deliveries").Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("uncommitted export escaped: %d %v", visible, err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if result, err := usage.PersistEvent(t.Context(), h.Pool, event, payload); err != nil || result.Outcome != usage.PersistOutcomeDuplicate {
		t.Fatalf("duplicate source: %v %v", result, err)
	}
	var count int
	if err = h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.export_deliveries").Scan(&count); err != nil || count != 3 {
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
	if _, err = h.Pool.Exec(t.Context(), "UPDATE olp.export_deliveries SET next_attempt_at=now() WHERE status='pending'"); err != nil {
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
	if err = h.Pool.QueryRow(t.Context(), "SELECT failed_total,delivered_total FROM olp.export_sinks WHERE id=$1", global["id"]).Scan(&failed, &delivered); err != nil || failed != 2 || delivered != 2 {
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
	if err = h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.export_deliveries WHERE stream='attempts' AND status='delivered'").Scan(&attempts); err != nil || attempts != 2 {
		t.Fatalf("attempt/correction exports %d %v", attempts, err)
	}
	// Expired pending payloads record gaps independently of fact retention.
	if _, err = h.Pool.Exec(t.Context(), "INSERT INTO olp.export_deliveries(sink_id,event_id,stream,payload,expires_at) VALUES($1,$2,'requests','{}',now()-interval '1 second')", global["id"], uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err = worker.Deliver(t.Context()); err != nil {
		t.Fatal(err)
	}
	var expired int
	if err = h.Pool.QueryRow(t.Context(), "SELECT expired_total FROM olp.export_sinks WHERE id=$1", global["id"]).Scan(&expired); err != nil || expired != 1 {
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
	if err = h.Pool.QueryRow(t.Context(), "SELECT delivered_total FROM olp.export_sinks WHERE id=$1", auditSink["id"]).Scan(&auditCount); err != nil || auditCount < 2 {
		t.Fatalf("audit delivery %d %v", auditCount, err)
	}
	_ = projectSink
}

func TestExportSinkPromotionBindsDestinationSecretsAndReusesUnchangedDefinitions(t *testing.T) {
	source, destination := newAccessHarness(t), newAccessHarness(t)
	a, b := source.owner(), destination.owner()
	const sourceSecret = "source-binding-do-not-copy"
	created := source.want(a, "POST", "/api/v1/sinks", map[string]any{"name": "Portable facts", "type": "https", "destination": "http://127.0.0.1:4199/export", "streams": []string{"requests"}, "enabled": false, "credential": sourceSecret}, idem(uuid.NewString()), 201)
	exported := source.want(a, "GET", "/api/v1/configuration/export", nil, nil, 200)
	doc := exported["document"].(map[string]any)
	entry := doc["sinks"].([]any)[0].(map[string]any)
	ref := entry["credential_ref"].(string)
	encoded, _ := json.Marshal(entry)
	if strings.Contains(string(encoded), sourceSecret) || strings.Contains(string(encoded), created["id"].(string)) {
		t.Fatal("sink export includes local secret/identity")
	}
	unbound := destination.want(b, "POST", "/api/v1/configuration/plan", map[string]any{"document": doc}, nil, 200)
	if len(unbound["blockers"].([]any)) == 0 {
		t.Fatal("unbound signing credential was silently lost")
	}
	bindings := map[string]string{ref: "destination-sink-signing-material"}
	destination.want(b, "POST", "/api/v1/configuration/plan", map[string]any{"document": doc, "secret_bindings": bindings}, nil, 200)
	destination.want(b, "POST", "/api/v1/configuration/apply", map[string]any{"document": doc, "secret_bindings": bindings}, idem(uuid.NewString()), 200)
	var id, etag, credential string
	if err := destination.Pool.QueryRow(t.Context(), "SELECT id::text,etag::text,credential_id::text FROM olp.export_sinks WHERE name='Portable facts'").Scan(&id, &etag, &credential); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing([]byte(destination.Ring))
	if err != nil {
		t.Fatal(err)
	}
	installation, err := database.Installation(t.Context(), destination.Pool)
	if err != nil {
		t.Fatal(err)
	}
	material, err := ring.Read(t.Context(), destination.Pool, installation, credential, secrets.SinkCredential)
	if err != nil || string(material) != bindings[ref] {
		t.Fatalf("destination binding was not selected: %v", err)
	}
	clear(material)
	reuse := destination.want(b, "POST", "/api/v1/configuration/plan", map[string]any{"document": doc}, nil, 200)
	found := false
	for _, raw := range reuse["actions"].([]any) {
		item := raw.(map[string]any)
		if item["kind"] == "sink" {
			found = true
			if item["action"] != "reuse" {
				t.Fatalf("unchanged sink action: %v", item)
			}
		}
	}
	if !found {
		t.Fatal("missing sink reuse evidence")
	}
	destination.want(b, "POST", "/api/v1/configuration/apply", map[string]any{"document": doc}, idem(uuid.NewString()), 200)
	var after string
	if err = destination.Pool.QueryRow(t.Context(), "SELECT etag::text FROM olp.export_sinks WHERE id=$1", id).Scan(&after); err != nil || after != etag {
		t.Fatalf("reused sink mutated: %v", err)
	}
	docEntry := doc["sinks"].([]any)[0].(map[string]any)
	delete(docEntry, "credential_ref")
	destination.want(b, "POST", "/api/v1/configuration/apply", map[string]any{"document": doc}, idem(uuid.NewString()), 200)
	var remaining int
	if err = destination.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.secrets WHERE id=$1", credential).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("removed signing credential remains: %d %v", remaining, err)
	}
}
