//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/export"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/usage"
)

func captureDefinition(t *testing.T, h *accessHarness, owner *browser, project, name, kind string, config map[string]any, activate bool) map[string]any {
	t.Helper()
	d := h.want(owner, "POST", "/api/v1/guardrails", map[string]any{"name": name, "project_id": project, "type": kind, "configuration": config, "timeout_ms": 1000, "on_error": "fail_open"}, idem(uuid.NewString()), 201)
	if activate {
		h.want(owner, "POST", "/api/v1/guardrails/"+d["id"].(string)+"/activate", map[string]any{"revision_id": d["revision_id"]}, map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + d["etag"].(string) + `"`}, 204)
		d = h.want(owner, "GET", "/api/v1/guardrails/"+d["id"].(string), nil, nil, 200)
	}
	return d
}
func TestCaptureRedactorPublicationPinsAndMigration(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Capture redaction")
	other := createProject(h, owner, "Other capture")
	fixtureRoute(t, h, "capture-pins")
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.routes SET project_id=$1 WHERE slug='capture-pins'`, project); err != nil {
		t.Fatal(err)
	}
	h.want(owner, "PATCH", "/api/v1/observability/capture", map[string]any{"enabled": true}, etagHeader(h.want(owner, "GET", "/api/v1/observability/capture", nil, nil, 200)), 200)
	_, server := newExportReceiver(t, "", 204)
	sink := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{"name": "capture-pins", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json"}, idem(uuid.NewString()), 201)
	config := map[string]any{"words": []string{"secret"}}
	d := captureDefinition(t, h, owner, project, "private", "builtin.keywords", config, true)
	captureDefinition(t, h, owner, other, "private", "builtin.keywords", config, true)
	captureDefinition(t, h, owner, other, "foreign", "builtin.keywords", config, true)
	captureDefinition(t, h, owner, project, "draft", "builtin.keywords", config, false)
	captureDefinition(t, h, owner, project, "schema", "builtin.schema", map[string]any{"schema": map[string]any{"type": "object"}}, true)
	retired := captureDefinition(t, h, owner, project, "retired", "builtin.keywords", config, true)
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.guardrails SET retired_at=now() WHERE id=$1`, retired["id"]); err != nil {
		t.Fatal(err)
	}
	body := func(names []string) map[string]any {
		return map[string]any{"route_slug": "capture-pins", "sink": sink["export_sink_id"], "sample_ratio": "1", "include": []string{"output"}, "max_bytes": 65536, "redact": names}
	}
	for _, name := range []string{"missing", "foreign", "draft", "schema", "retired"} {
		h.want(owner, "POST", "/api/v1/observability/capture-policies", body([]string{name}), idem(uuid.NewString()), 422)
	}
	p := h.want(owner, "POST", "/api/v1/observability/capture-policies", body([]string{"private"}), idem(uuid.NewString()), 201)
	id := p["id"].(string)
	endpoint := "/api/v1/observability/capture-policies/" + id
	pin := func(policy map[string]any) string {
		return policy["redactor_revisions"].([]any)[0].(map[string]any)["revision_id"].(string)
	}
	if pin(p) != d["revision_id"] {
		t.Fatal("route-only policy crossed project or pinned draft")
	}
	// Replacing the former empty-array constraint must preserve existing policies
	// and make both nonempty lists and empty lists legal after migration 0074.
	var count int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.capture_redactors WHERE policy_id=$1 AND position=0 AND revision_id=$2`, id, d["revision_id"]).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration binding: %d %v", count, err)
	}
	d2 := h.want(owner, "PUT", "/api/v1/guardrails/"+d["id"].(string), map[string]any{"name": "private", "configuration": map[string]any{"words": []string{"new-secret"}}, "timeout_ms": 1000, "on_error": "fail_open"}, map[string]string{"If-Match": `"` + d["etag"].(string) + `"`, "Idempotency-Key": uuid.NewString()}, 200)
	h.want(owner, "POST", "/api/v1/guardrails/"+d["id"].(string)+"/activate", map[string]any{"revision_id": d2["revision_id"]}, map[string]string{"If-Match": `"` + d2["etag"].(string) + `"`, "Idempotency-Key": uuid.NewString()}, 204)
	p = h.want(owner, "PATCH", endpoint, map[string]any{"sample_ratio": "0.5"}, etagHeader(p), 200)
	if pin(p) != d["revision_id"] {
		t.Fatal("unrelated edit adopted active revision")
	}
	p = h.want(owner, "PATCH", endpoint, map[string]any{"redact": []string{"private"}}, etagHeader(p), 200)
	if pin(p) != d2["revision_id"] {
		t.Fatal("explicit refresh did not adopt revision")
	}
	h.want(owner, "PATCH", endpoint, map[string]any{"redactor_revisions": []any{}}, etagHeader(p), 400)
	p = h.want(owner, "PATCH", endpoint, map[string]any{"redact": []string{}}, etagHeader(p), 200)
	if len(p["redactor_revisions"].([]any)) != 0 {
		t.Fatal("cleared list retained bindings")
	}
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.capture_redactors WHERE policy_id=$1`, id).Scan(&count); err != nil || count != 0 {
		t.Fatal("cleared binding rows retained")
	}
	// Replay the actual forward migration over an empty-redaction policy. Its
	// etag and settings must survive the upgrade unchanged.
	if _, err := h.Pool.Exec(t.Context(), `DROP TABLE olp.capture_redactors; ALTER TABLE olp.capture_policies DROP CONSTRAINT capture_policies_redact_check; ALTER TABLE olp.capture_policies ADD CONSTRAINT capture_policies_redact_check CHECK(cardinality(redact)=0)`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../internal/database/migrations/0074_capture_redactors.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Pool.Exec(t.Context(), string(migration)); err != nil {
		t.Fatal(err)
	}
	after := h.want(owner, "GET", endpoint, nil, nil, 200)
	if after["etag"] != p["etag"] || after["sample_ratio"] != p["sample_ratio"] || len(after["redactor_revisions"].([]any)) != 0 {
		t.Fatal("migration changed an existing policy")
	}
}

func TestCaptureRedactedGatewayReadback(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	h.Gateway.Sink = &gateway.PersistingSink{Persist: func(ctx context.Context, event *usage.Event, payload []byte) error {
		_, err := usage.PersistEvent(ctx, h.Pool, event, payload)
		return err
	}}
	owner := h.owner()
	project := createProject(h, owner, "Redacted gateway")
	_, _, slug, secret := provisionOpenAIInProject(t, h, owner, &project, fixture.URL, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}}, []string{"generation"}, nil)
	captureDefinition(t, h, owner, project, "private", "builtin.regex", map[string]any{"rules": []any{map[string]any{"id": "private", "pattern": "capture-sentinel|OK", "replacement": "[REMOVED]"}}}, true)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.want(owner, "PATCH", "/api/v1/observability/capture", map[string]any{"enabled": true}, etagHeader(h.want(owner, "GET", "/api/v1/observability/capture", nil, nil, 200)), 200)
	receiver, server := newExportReceiver(t, "", 204)
	sink := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{"name": "redacted-target", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json"}, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/observability/capture-policies", map[string]any{"route_slug": slug, "sink": sink["export_sink_id"], "sample_ratio": "1", "include": []string{"input", "output", "tool_calls"}, "redact": []string{"private"}, "max_bytes": 65536}, idem(uuid.NewString()), 201)
	manager := &export.Manager{Pool: h.Pool, Keys: h.Ring2(), Installation: h.Server.Installation, Sender: export.NewSender(loopbackPolicy()), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h.Gateway.Capture = manager
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { manager.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(3 * time.Second)
	for {
		c := manager.Resolve(project, slug, "", "", uuid.NewString())
		if c != nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("capture cache did not compile")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, stream := range []bool{false, true} {
		body := `{"model":"` + slug + `","messages":[{"role":"user","content":"capture-sen\u0074inel"}],"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`
		status, raw, _ := h.gatewayRaw("POST", "/v1/chat/completions", secret, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
		if status != http.StatusOK || !strings.Contains(string(raw), "OK") || strings.Contains(string(raw), "[REMOVED]") {
			t.Fatalf("inference changed: %d %s", status, raw)
		}
	}
	for _, env := range awaitEnvelope(t, receiver, 2) {
		raw, _ := json.Marshal(env["data"])
		if strings.Contains(string(raw), "capture-sentinel") || strings.Contains(string(raw), `\u0074`) || strings.Contains(string(raw), `"OK"`) || strings.Count(string(raw), "[REMOVED]") < 2 {
			t.Fatalf("sink readback: %s", raw)
		}
	}
	for _, query := range []string{
		`SELECT count(*) FROM olp.requests r WHERE to_json(r)::text LIKE '%capture-sentinel%' OR to_json(r)::text LIKE '%[REMOVED]%'`,
		`SELECT count(*) FROM olp.attempts a WHERE to_json(a)::text LIKE '%capture-sentinel%' OR to_json(a)::text LIKE '%[REMOVED]%'`,
		`SELECT count(*) FROM olp.audit a WHERE to_json(a)::text LIKE '%capture-sentinel%' OR to_json(a)::text LIKE '%[REMOVED]%'`,
		`SELECT count(*) FROM olp.export_records WHERE payload::text LIKE '%capture-sentinel%' OR payload::text LIKE '%[REMOVED]%'`,
	} {
		var count int
		if err := h.Pool.QueryRow(t.Context(), query).Scan(&count); err != nil || count != 0 {
			t.Fatalf("durable payload: count=%d err=%v", count, err)
		}
	}
}
