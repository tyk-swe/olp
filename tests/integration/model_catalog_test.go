//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func createCatalogRoute(h *accessHarness, owner *browser, projectID, slug string) string {
	h.t.Helper()
	vendor := newVendor(h.t)
	provider := createScopedProvider(h, owner, "Catalog "+slug, vendor.URL+"/v1", projectID, 201)
	activateScopedProvider(h, owner, provider)
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": slug, "project_id": projectID, "fidelity": map[string]any{"mode": "transformed"},
		"operations": []any{"generation"}, "overall_timeout_ms": 30000, "max_attempts": 1,
		"targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 2000}},
	}, idem("catalog-draft-"+slug), 201)
	draftID := draft["id"].(string)
	activated := h.want(owner, "POST", "/api/v1/route-drafts/"+draftID+"/activate", nil, withMatch(draft, idem("catalog-activate-"+slug)), 200)
	return activated["route_id"].(string)
}

func TestModelCatalogVisibilityPublishingAndUpstreamDisclosure(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	alpha, beta := createProject(h, owner, "Catalog alpha"), createProject(h, owner, "Catalog beta")
	routeID := createCatalogRoute(h, owner, alpha, "catalog-alpha")
	createCatalogRoute(h, owner, beta, "catalog-beta")
	h.want(owner, "POST", "/api/v1/pricing/revisions", map[string]any{"effective_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), "prices": []any{repPrice("openai_compatible", vendorModel, "generation")}}, idem("catalog-price"), 201)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	all := h.want(owner, "GET", "/api/v1/catalog", nil, nil, 200)
	if len(all["items"].([]any)) != 2 {
		t.Fatalf("owner catalog: %v", all)
	}
	for _, raw := range all["items"].([]any) {
		samples := raw.(map[string]any)["samples"].([]any)
		if len(samples) != 1 || samples[0].(map[string]any)["sdk"] != "openai" {
			t.Fatalf("catalog advertised an uncertified SDK: %v", samples)
		}
	}
	serialized, _ := json.Marshal(all)
	if strings.Contains(string(serialized), vendorModel) || strings.Contains(string(serialized), "Catalog catalog-") || strings.Contains(string(serialized), vendorSecret) {
		t.Fatal("default catalog disclosed provider identity or credentials")
	}
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Catalog consumer", "project_id": alpha, "scopes": []string{"models_read"}, "allowed_routes": []string{"catalog-alpha"}}, idem("catalog-consumer"), 201)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, _, body := h.call(sweepCaller{token: key["secret"].(string)}, "GET", "/api/v1/catalog", nil, nil)
	if status != 200 || !strings.Contains(body, "catalog-alpha") || strings.Contains(body, "catalog-beta") {
		t.Fatalf("key catalog: %d %s", status, body)
	}
	token := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "Catalog scoped member", "scopes": []any{"read"}, "project_ids": []string{alpha}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, idem("catalog-management-token"), 201)
	status, _, body = h.call(sweepCaller{token: token["secret"].(string)}, "GET", "/api/v1/catalog", nil, nil)
	if status != 200 || !strings.Contains(body, "catalog-alpha") || strings.Contains(body, "catalog-beta") {
		t.Fatalf("project-scoped catalog: %d %s", status, body)
	}
	publicationPath := "/api/v1/projects/" + alpha + "/catalog"
	publicPath := "/api/v1/catalog/public/" + alpha
	if status, _, _ = h.call(sweepCaller{}, "GET", publicPath, nil, nil); status != 404 {
		t.Fatalf("unpublished project: %d", status)
	}
	publication := h.want(owner, "GET", publicationPath, nil, nil, 200)
	h.want(owner, "PUT", publicationPath, map[string]any{"enabled": true, "prices_public": false}, nil, 428)
	publication = h.want(owner, "PUT", publicationPath, map[string]any{"enabled": true, "prices_public": false}, etagHeader(publication), 200)
	status, _, body = h.call(sweepCaller{}, "GET", publicPath, nil, nil)
	if status != 200 || !strings.Contains(body, "catalog-alpha") || strings.Contains(body, "catalog-beta") || strings.Contains(body, `"prices"`) || strings.Contains(body, vendorModel) {
		t.Fatalf("public catalog boundaries: %d %s", status, body)
	}
	exposurePath := "/api/v1/routes/" + routeID + "/catalog"
	exposure := h.want(owner, "GET", exposurePath, nil, nil, 200)
	h.want(owner, "PUT", exposurePath, map[string]any{"expose_upstream_models": true}, etagHeader(exposure), 200)
	h.want(owner, "PUT", exposurePath, map[string]any{"expose_upstream_models": false}, etagHeader(exposure), 412)
	status, _, body = h.call(sweepCaller{}, "GET", publicPath, nil, nil)
	if status != 200 || !strings.Contains(body, vendorModel) {
		t.Fatalf("explicit upstream opt-in: %d %s", status, body)
	}
	publication = h.want(owner, "PUT", publicationPath, map[string]any{"enabled": true, "prices_public": true}, etagHeader(publication), 200)
	if publication["prices_public"] != true {
		t.Fatal("independent price opt-in not retained")
	}
	status, _, body = h.call(sweepCaller{}, "GET", publicPath, nil, nil)
	if status != 200 || !strings.Contains(body, `"input_per_million"`) || !strings.Contains(body, `"minimum":"1.5"`) {
		t.Fatalf("public price opt-in: %d %s", status, body)
	}
	h.want(owner, "PUT", publicationPath, map[string]any{"enabled": false, "prices_public": false}, etagHeader(publication), 200)
	if status, _, _ = h.call(sweepCaller{}, "GET", publicPath, nil, nil); status != 404 {
		t.Fatalf("withdrawn project: %d", status)
	}
	keyDetail := h.want(owner, "GET", "/api/v1/api-keys/"+key["id"].(string), nil, nil, 200)
	headers := etagHeader(keyDetail)
	headers["Idempotency-Key"] = "revoke-catalog-consumer"
	h.want(owner, "POST", "/api/v1/api-keys/"+key["id"].(string)+"/revoke", nil, headers, 200)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if status, _, _ = h.call(sweepCaller{token: key["secret"].(string)}, http.MethodGet, "/api/v1/catalog", nil, nil); status != 401 {
		t.Fatalf("revoked catalog consumer: %d", status)
	}
}
