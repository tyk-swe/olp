//go:build integration

package integration_test

import (
	"net/http"
	"testing"
)

func TestRouteRetirementStopsServingAndRestores(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	vendor := newVendor(t)
	created := h.want(owner, "POST", "/api/v3/providers", map[string]any{"name": "Lifecycle vendor", "configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": vendor.URL + "/v1/"}, "credential": vendorSecret}, map[string]string{"Idempotency-Key": "provider"}, 201)
	providerPath := "/api/v3/providers/" + created["id"].(string)
	detail := h.want(owner, "GET", providerPath, nil, nil, 200)
	probe := h.want(owner, "POST", providerPath+"/probe", nil, etagHeader(detail), 200)
	if probe["succeeded"] != true {
		t.Fatalf("probe %v", probe)
	}
	detail = h.want(owner, "POST", providerPath+"/discovery", map[string]any{"models": []any{}}, etagHeader(detail), 200)
	var modelID string
	for _, item := range h.want(owner, "GET", providerPath+"/models", nil, nil, 200)["items"].([]any) {
		if item.(map[string]any)["upstream_model"] == vendorModel {
			modelID = item.(map[string]any)["id"].(string)
		}
	}
	modelPath := providerPath + "/models/" + modelID
	capabilities := []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}}
	detail = h.want(owner, "PATCH", modelPath, map[string]any{"enabled": true, "capabilities": capabilities}, etagHeader(detail), 200)
	certified := h.want(owner, "POST", modelPath+"/certify", nil, etagHeader(detail), 200)
	if certified["status"] != "certified" {
		t.Fatalf("certification %v", certified)
	}
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "activate"}), 200)

	publish := func(key string) map[string]any {
		t.Helper()
		draft := h.want(owner, "POST", "/api/v3/route-drafts", map[string]any{
			"slug": routeSlug, "overall_timeout_ms": 5000, "max_attempts": 1,
			"targets": []any{map[string]any{"provider_id": created["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 2000}},
		}, map[string]string{"Idempotency-Key": "draft-" + key}, 201)
		draftPath := "/api/v3/route-drafts/" + draft["id"].(string)
		validated := h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draft), 200)
		return h.want(owner, "POST", draftPath+"/activate", nil, withMatch(validated, map[string]string{"Idempotency-Key": "activate-" + key}), 200)
	}
	activated := publish("initial")
	routePath := "/api/v3/routes/" + activated["route_id"].(string)

	key := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "inference", "scopes": []string{"inference"}}, map[string]string{"Idempotency-Key": "key"}, 201)
	chat := map[string]any{"model": routeSlug, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	h.refresh()
	if status, body, _ := h.gateway("POST", "/v1/chat/completions", key["secret"].(string), chat); status != http.StatusOK {
		t.Fatalf("published route must serve: %d %v", status, body)
	}

	route := h.want(owner, "GET", routePath, nil, nil, 200)
	if route["state"] != "active" || route["retired_at"] != nil || route["retired_by"] != nil {
		t.Fatalf("active route %v", route)
	}
	etag, _ := route["etag"].(string)
	if etag == "" {
		t.Fatalf("route without an etag: %v", route)
	}

	if problem := h.want(owner, "POST", routePath+"/retire", nil, map[string]string{"Idempotency-Key": "retire-missing"}, 428); problemCode(t, problem) != "precondition_required" {
		t.Fatalf("missing If-Match: %v", problem)
	}
	if problem := h.want(owner, "POST", routePath+"/retire", nil, map[string]string{"If-Match": `"00000000-0000-0000-0000-000000000000"`, "Idempotency-Key": "retire-stale"}, 412); problemCode(t, problem) != "etag_mismatch" {
		t.Fatalf("stale etag: %v", problem)
	}
	retired := h.want(owner, "POST", routePath+"/retire", nil, map[string]string{"If-Match": `"` + etag + `"`, "Idempotency-Key": "retire"}, 200)
	if retired["etag"] == etag {
		t.Fatalf("retirement must rotate the route etag: %v", retired)
	}

	h.refresh()
	if status, body, _ := h.gateway("POST", "/v1/chat/completions", key["secret"].(string), chat); status != http.StatusNotFound || h.gatewayCode(status, body) != "route_not_found" {
		t.Fatalf("retired route still served: %d %v", status, body)
	}
	route = h.want(owner, "GET", routePath, nil, nil, 200)
	if route["state"] != "retired" || route["retired_at"] == nil || route["retired_by"] == nil {
		t.Fatalf("retired route %v", route)
	}
	if revisions := h.want(owner, "GET", routePath+"/revisions", nil, nil, 200); len(revisions["items"].([]any)) != 1 {
		t.Fatalf("retirement must not touch revisions: %v", revisions)
	}
	if problem := h.want(owner, "POST", routePath+"/retire", nil, map[string]string{"If-Match": `"` + retired["etag"].(string) + `"`, "Idempotency-Key": "retire-again"}, 409); problemCode(t, problem) != "route_retired" {
		t.Fatalf("double retirement: %v", problem)
	}

	restored := publish("restore")
	route = h.want(owner, "GET", routePath, nil, nil, 200)
	if route["state"] != "active" || route["retired_at"] != nil || route["retired_by"] != nil || route["etag"] == retired["etag"] {
		t.Fatalf("restored route %v", route)
	}
	if restored["revision"] != float64(2) {
		t.Fatalf("restoration must continue the revision history: %v", restored)
	}
	h.refresh()
	if status, body, _ := h.gateway("POST", "/v1/chat/completions", key["secret"].(string), chat); status != http.StatusOK {
		t.Fatalf("restored route must serve again: %d %v", status, body)
	}
}
