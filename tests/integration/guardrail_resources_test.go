//go:build integration

package integration_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func namedPolicy(pattern string) map[string]any {
	return map[string]any{"rules": []any{map[string]any{"id": "private", "phase": "input", "action": "block", "pattern": pattern}}}
}

func TestNamedGuardrailRevisionsRemainScopedAndPublishedCopiesStayPinned(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Guardrail project")
	other := createProject(h, owner, "Other guardrails")
	created := h.want(owner, "POST", "/api/v1/guardrails", map[string]any{"name": "Private filter", "project_id": project, "type": "builtin.regex", "policy": namedPolicy("restricted")}, idem(uuid.NewString()), 201)
	id := created["id"].(string)
	path := "/api/v1/guardrails/" + id
	validateManagementResponse(t, "POST", "/api/v1/guardrails", 201, created)
	originalRevision := created["revision_id"].(string)
	h.want(owner, "PUT", path, map[string]any{"name": "Changed", "policy": namedPolicy("different")}, idem(uuid.NewString()), 428)
	h.want(owner, "PUT", path, map[string]any{"name": "Changed", "policy": namedPolicy("different")}, map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + uuid.NewString() + `"`}, 412)
	policyCaller := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "Other project", "scopes": []any{"read", "configure"}, "project_ids": []any{other}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, idem(uuid.NewString()), 201)
	caller := sweepCaller{token: policyCaller["secret"].(string)}
	if status, _, _ := h.call(caller, "GET", path, nil, nil); status != 404 {
		t.Fatalf("foreign definition visible: %d", status)
	}
	if status, _, _ := h.call(caller, "GET", path+"/revisions/"+originalRevision, nil, nil); status != 404 {
		t.Fatalf("foreign revision visible: %d", status)
	}
	if status, _, _ := h.call(caller, "PUT", path, map[string]any{"name": "Changed", "policy": namedPolicy("different")}, map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + created["etag"].(string) + `"`}); status != 404 {
		t.Fatalf("foreign definition writable: %d", status)
	}

	routeID := createCatalogRoute(h, owner, project, "guardrail-pinned")
	route := h.want(owner, "GET", "/api/v1/routes/"+routeID, nil, nil, 200)
	revision := route["latest_revision"].(map[string]any)
	input := map[string]any{"slug": "guardrail-pinned", "operations": revision["operations"], "overall_timeout_ms": revision["overall_timeout_ms"], "max_attempts": revision["max_attempts"], "targets": []any{map[string]any{"provider_model_id": revision["targets"].([]any)[0].(map[string]any)["provider_model_id"], "priority": 0, "weight": 1, "timeout_ms": 2000}}, "fidelity": map[string]any{"mode": "transformed"}, "content_policy": created["policy"]}
	h.want(owner, "PUT", "/api/v1/routes/"+routeID, input, map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + route["etag"].(string) + `"`}, 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Guardrail client", "project_id": project, "scopes": []any{"inference"}, "allowed_routes": []any{"guardrail-pinned"}}, idem(uuid.NewString()), 201)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if status, _, raw := h.call(sweepCaller{token: key["secret"].(string)}, "POST", "/v1/chat/completions", map[string]any{"model": "guardrail-pinned", "messages": []any{map[string]any{"role": "user", "content": "restricted"}}}, nil); status != 400 || !strings.Contains(raw, "content_policy_blocked") {
		t.Fatalf("published policy not enforced: %d %s", status, raw)
	}
	headers := map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + created["etag"].(string) + `"`}
	updated := h.want(owner, "PUT", path, map[string]any{"name": "Private filter", "policy": namedPolicy("different")}, headers, 200)
	if updated["revision"].(float64) != 2 || updated["revision_id"] == originalRevision {
		t.Fatal("definition did not append a revision")
	}
	if replay := h.want(owner, "PUT", path, map[string]any{"name": "Private filter", "policy": namedPolicy("different")}, headers, 200); replay["revision_id"] != updated["revision_id"] {
		t.Fatal("replay appended a revision")
	}
	old := h.want(owner, "GET", path+"/revisions/"+originalRevision, nil, nil, 200)
	encoded, _ := json.Marshal(old["policy"])
	if !strings.Contains(string(encoded), "restricted") || strings.Contains(string(encoded), "different") {
		t.Fatal("old policy revision changed")
	}
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.guardrail_revisions SET policy='{}' WHERE id=$1", originalRevision); err == nil {
		t.Fatal("immutable revision allowed mutation")
	}
	retireHeaders := map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + updated["etag"].(string) + `"`}
	h.want(owner, "DELETE", path, nil, retireHeaders, 204)
	h.want(owner, "DELETE", path, nil, retireHeaders, 204)
	retired := h.want(owner, "GET", path, nil, nil, 200)
	if retired["retired_at"] == nil {
		t.Fatal("definition not retired")
	}
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if policy := h.Runtime.Release().Snapshot.Routes["guardrail-pinned"].ContentPolicy; policy == nil || policy.Rules[0].Pattern != "restricted" {
		t.Fatal("retirement changed a published copy")
	}
	var audits int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.audit WHERE resource_id=$1 AND action LIKE 'guardrail.%'", id).Scan(&audits); err != nil || audits != 3 {
		t.Fatalf("mutation audits: %d %v", audits, err)
	}
}

func TestNamedGuardrailConfigurationPromotionIsPortableAndIdempotent(t *testing.T) {
	source, destination := newAccessHarness(t), newAccessHarness(t)
	owner, destinationOwner := source.owner(), destination.owner()
	project := createProject(source, owner, "Portable guardrails")
	source.want(owner, "POST", "/api/v1/guardrails", map[string]any{"name": "Private filter", "project_id": project, "type": "builtin.regex", "policy": namedPolicy("restricted")}, idem(uuid.NewString()), 201)
	exported := source.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	doc := exported["document"].(map[string]any)
	entry := doc["guardrails"].([]any)[0].(map[string]any)
	if entry["project"] != "Portable guardrails" || entry["name"] != "Private filter" || entry["id"] != nil || entry["project_id"] != nil || entry["revision_id"] != nil {
		t.Fatalf("definition is not portable: %v", entry)
	}
	promotion := map[string]any{"document": doc}
	plan := destination.want(destinationOwner, "POST", "/api/v1/configuration/plan", promotion, nil, 200)
	validateManagementResponse(t, "POST", "/api/v1/configuration/plan", 200, plan)
	destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", promotion, idem(uuid.NewString()), 200)
	listing := destination.want(destinationOwner, "GET", "/api/v1/guardrails", nil, nil, 200)
	if len(listing["items"].([]any)) != 1 {
		t.Fatal("definition was not promoted")
	}
	repeated := destination.want(destinationOwner, "POST", "/api/v1/configuration/plan", promotion, nil, 200)
	for _, raw := range repeated["actions"].([]any) {
		if action := raw.(map[string]any); action["kind"] == "guardrail" && action["action"] != "reuse" {
			t.Fatalf("definition did not plan as reusable: %v", action)
		}
	}
	destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", promotion, idem(uuid.NewString()), 200)
	var versions int
	if err := destination.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.guardrail_revisions").Scan(&versions); err != nil || versions != 1 {
		t.Fatalf("repeat promotion appended a revision: %d %v", versions, err)
	}
}
