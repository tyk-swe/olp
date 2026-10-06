//go:build integration

package integration_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/catalog"
)

// TestCatalogFactsAreSuggestedAndAcceptedAsOperatorFacts covers discovery's
// catalog suggestions end to end: a provider's model matches the reference
// catalog, accepting stores provenance-tagged operator facts, and because that
// changes the configuration, certified capabilities return to declared.
func TestCatalogFactsAreSuggestedAndAcceptedAsOperatorFacts(t *testing.T) {
	signed, err := catalog.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	var model string
	for _, vendor := range signed.Catalog.Vendors {
		if vendor.ID != "openai" {
			continue
		}
		for _, candidate := range vendor.Models {
			if candidate.ContextLength != nil && candidate.Prices[0].Operation == "generation" {
				model = candidate.ID
				break
			}
		}
	}
	if model == "" {
		t.Fatal("the reference catalog has no OpenAI generation model with a context length")
	}
	h := newAccessHarness(t)
	owner := h.owner()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/responses" {
			writeResponsesFixture(w, model, "ok", false)
			return
		}
		fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`, model)
	}))
	t.Cleanup(up.Close)
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Catalog facts", "configuration": map[string]any{"kind": "openai", "auth_mode": "api_key", "endpoint": up.URL + "/v1"}, "credential": vendorSecret, "model": model},
		map[string]string{"Idempotency-Key": "catalog-provider"}, 201)
	providerPath := "/api/v1/providers/" + created["id"].(string)

	models := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)["items"].([]any)
	modelPath := providerPath + "/models/" + models[0].(map[string]any)["id"].(string)
	detail := h.want(owner, "GET", providerPath, nil, nil, 200)
	detail = h.want(owner, "PATCH", modelPath, map[string]any{"enabled": true, "capabilities": []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}}, etagHeader(detail), 200)
	if certified := h.want(owner, "POST", modelPath+"/certify", nil, etagHeader(detail), 200); certified["certified_count"] != float64(1) {
		t.Fatalf("certification = %v", certified)
	}

	suggestions := h.want(owner, "GET", providerPath+"/catalog-suggestions", nil, nil, 200)
	items := suggestions["items"].([]any)
	if suggestions["vendor_id"] != "openai" || len(items) != 1 || items[0].(map[string]any)["upstream_model"] != model || len(items[0].(map[string]any)["changes"].([]any)) == 0 {
		t.Fatalf("suggestions = %v", suggestions)
	}
	digest := suggestions["catalog"].(map[string]any)["sha256"].(string)

	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	status, problem, _ := h.request(owner, "POST", providerPath+"/catalog-suggestions/accept", map[string]any{"catalog_sha256": strings.Repeat("0", 64), "upstream_models": []string{model}},
		withMatch(detail, map[string]string{"Idempotency-Key": "stale-catalog"}))
	if status != 409 {
		t.Fatalf("accepting against another catalog = %d %v", status, problem)
	}
	status, problem, _ = h.request(owner, "POST", providerPath+"/catalog-suggestions/accept", map[string]any{"catalog_sha256": digest, "upstream_models": []string{"not-in-catalog"}},
		withMatch(detail, map[string]string{"Idempotency-Key": "unmatched"}))
	if status != 422 {
		t.Fatalf("accepting an unmatched model = %d %v", status, problem)
	}
	accepted := h.want(owner, "POST", providerPath+"/catalog-suggestions/accept", map[string]any{"catalog_sha256": digest, "upstream_models": []string{model}},
		withMatch(detail, map[string]string{"Idempotency-Key": "accept"}), 200)
	facts := accepted["configuration"].(map[string]any)["options"].(map[string]any)["models"].(map[string]any)[model].(map[string]any)
	if facts["source"] != "catalog@"+digest || facts["context_length"] == nil {
		t.Fatalf("stored facts = %v", facts)
	}
	capabilities := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)["items"].([]any)[0].(map[string]any)["capabilities"].([]any)
	for _, c := range capabilities {
		if c.(map[string]any)["source"] != "declared" {
			t.Fatalf("a capability stayed certified after the configuration changed: %v", capabilities)
		}
	}
	after := h.want(owner, "GET", providerPath+"/catalog-suggestions", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	if len(after["changes"].([]any)) != 0 {
		t.Fatalf("accepted facts still differ: %v", after)
	}
}
