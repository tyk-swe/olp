//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestPublishedRouteWritesAreConditionalAtomicAndPreserveIndependentDrafts(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Published automation")
	vendor := newVendor(t)
	provider := createScopedProvider(h, owner, "Published provider", vendor.URL+"/v1", project, 201)
	activateScopedProvider(h, owner, provider)
	input := map[string]any{
		"slug": "published-automation", "project_id": project,
		"fidelity": map[string]any{"mode": "transformed"}, "operations": []any{"generation"},
		"overall_timeout_ms": 30000, "max_attempts": 1,
		"targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 2000}},
	}
	independent := h.want(owner, "POST", "/api/v1/route-drafts", input, idem(uuid.NewString()), 201)
	created := h.want(owner, "POST", "/api/v1/routes", input, idem("atomic-create"), 201)
	validateManagementResponse(t, "POST", "/api/v1/routes", 201, created)
	id := created["id"].(string)
	path := "/api/v1/routes/" + id
	replayed := h.want(owner, "POST", "/api/v1/routes", input, idem("atomic-create"), 201)
	if replayed["id"] != id {
		t.Fatal("creation replay published another route")
	}
	h.want(owner, "POST", "/api/v1/routes", input, idem(uuid.NewString()), 409)
	response, raw := h.do(owner, "GET", path, nil, nil)
	if response.Header.Get("ETag") != `"`+created["etag"].(string)+`"` || response.StatusCode != 200 {
		t.Fatal("published read did not return its own observed ETag")
	}
	response.Body.Close()
	var observed map[string]any
	if err := json.Unmarshal(raw, &observed); err != nil {
		t.Fatal(err)
	}
	input["overall_timeout_ms"] = 40000
	h.want(owner, "PUT", path, input, idem(uuid.NewString()), 428)
	h.want(owner, "PUT", path, input, map[string]string{"If-Match": `"` + uuid.NewString() + `"`, "Idempotency-Key": uuid.NewString()}, 412)
	headers := withMatch(observed, idem("atomic-update"))
	updated := h.want(owner, "PUT", path, input, headers, 200)
	validateManagementResponse(t, "PUT", "/api/v1/routes/{route_id}", 200, updated)
	if updated["revision_count"] != float64(2) || updated["latest_revision"].(map[string]any)["overall_timeout_ms"] != float64(40000) {
		t.Fatal("replacement did not publish an immutable new revision")
	}
	h.want(owner, "PUT", path, input, headers, 200)
	h.want(owner, "PUT", path, input, withMatch(observed, idem(uuid.NewString())), 412)
	input["slug"] = "renamed"
	h.want(owner, "PUT", path, input, withMatch(updated, idem(uuid.NewString())), 422)
	input["slug"] = "published-automation"
	input["targets"] = []any{map[string]any{"provider_id": provider["id"], "provider_model": "unknown-model", "priority": 0, "weight": 1, "timeout_ms": 2000}}
	h.want(owner, "PUT", path, input, withMatch(updated, idem(uuid.NewString())), 422)
	current := h.want(owner, "GET", path, nil, nil, 200)
	if current["etag"] != updated["etag"] || current["revision_count"] != float64(2) {
		t.Fatal("failed replacement changed published state")
	}
	draft := h.want(owner, "GET", "/api/v1/route-drafts/"+independent["id"].(string), nil, nil, 200)
	if draft["etag"] != independent["etag"] || draft["overall_timeout_ms"] != float64(30000) {
		t.Fatal("automation overwrote an independent console draft")
	}
	var revisions, audits, drafts int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.route_revisions WHERE route_id=$1", id).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.audit WHERE action IN ('route.create','route.update') AND resource_id=$1", id).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.route_drafts WHERE slug='published-automation'").Scan(&drafts); err != nil {
		t.Fatal(err)
	}
	if revisions != 2 || audits != 2 || drafts != 3 {
		t.Fatalf("atomic/replay inventory: revisions=%d audits=%d drafts=%d", revisions, audits, drafts)
	}
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	served := h.Runtime.Release().Snapshot.Routes["published-automation"]
	if served.Revision != 2 || served.OverallTimeout != 40000 {
		t.Fatal("the published replacement did not reach the serving snapshot")
	}
	h.want(owner, "POST", path+"/retire", nil, withMatch(updated, idem(uuid.NewString())), http.StatusOK)
}

func TestPublishedRouteReplacementPreservesItsPolicy(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Policy preservation")
	id := createCatalogRoute(h, owner, project, "preserve-policy")
	path := "/api/v1/routes/" + id
	route := h.want(owner, "GET", path, nil, nil, 200)
	revision := route["latest_revision"].(map[string]any)
	draftID := revision["source_draft_id"].(string)
	draft := h.want(owner, "GET", "/api/v1/route-drafts/"+draftID, nil, nil, 200)
	policy := h.want(owner, "PUT", "/api/v1/routing-policies/route-draft/"+draftID, map[string]any{"allowed_strategies": []any{"weighted"}}, withMatch(draft, idem(uuid.NewString())), 200)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draftID+"/activate", nil, withMatch(policy, idem(uuid.NewString())), 200)
	route = h.want(owner, "GET", path, nil, nil, 200)
	revision = route["latest_revision"].(map[string]any)
	input := map[string]any{"slug": "preserve-policy", "operations": []any{"generation"}, "overall_timeout_ms": 40000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}}
	target := revision["targets"].([]any)[0].(map[string]any)
	input["targets"] = []any{map[string]any{"provider_model_id": target["provider_model_id"], "priority": 0, "weight": 1, "timeout_ms": 2000}}
	updated := h.want(owner, "PUT", path, input, withMatch(route, idem(uuid.NewString())), 200)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	served := h.Runtime.Release().Snapshot.Routes["preserve-policy"]
	if served.Policy == nil || len(served.Policy.AllowedStrategies) != 1 || served.Policy.AllowedStrategies[0] != "weighted" || served.Revision != 3 || updated["revision_count"] != float64(3) {
		t.Fatal("replacement discarded the existing published policy")
	}
}
