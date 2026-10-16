//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// A later edit must remain intact when the original rotation is retried.

// Sharing a credential across slots must still produce one history row.

func TestProviderInventoryAvailabilityTracksPublishedModels(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/v1/responses" {
			writeResponsesFixture(w, input.Model, "ok", false)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`, input.Model)
	}))
	t.Cleanup(up.Close)
	configuration := func(endpoint string) map[string]any {
		return map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": endpoint}
	}
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Inventory publication", "configuration": configuration(up.URL + "/v1"), "credential": vendorSecret, "model": "first"}, map[string]string{"Idempotency-Key": "provider"}, 201)
	providerPath := "/api/v1/providers/" + created["id"].(string)
	certify := func(name string) string {
		t.Helper()
		models := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)
		var id string
		for _, item := range models["items"].([]any) {
			m := item.(map[string]any)
			if m["upstream_model"] == name {
				id = m["id"].(string)
			}
		}
		if id == "" {
			t.Fatalf("missing model %s", name)
		}
		detail := h.want(owner, "GET", providerPath, nil, nil, 200)
		modelPath := providerPath + "/models/" + id
		detail = h.want(owner, "PATCH", modelPath, map[string]any{"enabled": true, "capabilities": []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}}, etagHeader(detail), 200)
		certified := h.want(owner, "POST", modelPath+"/certify", nil, etagHeader(detail), 200)
		if certified["status"] != "certified" || certified["certified_count"] != float64(1) {
			t.Fatalf("model %s certification: %v", name, certified)
		}
		return id
	}
	assertAvailability := func(want map[string]bool) {
		t.Helper()
		for _, query := range []string{"", "?surface=openai"} {
			inventory := h.want(owner, "GET", "/api/v1/provider-models"+query, nil, nil, 200)
			got := map[string]bool{}
			for _, item := range inventory["items"].([]any) {
				entry := item.(map[string]any)
				got[entry["model"].(map[string]any)["upstream_model"].(string)] = entry["available"].(bool)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("inventory%s availability %v, want %v", query, got, want)
			}
		}
	}
	firstID := certify("first")
	assertAvailability(map[string]bool{"first": false})
	detail := h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "publish-first"}), 200)
	assertAvailability(map[string]bool{"first": true})
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/discovery", map[string]any{"models": []any{map[string]any{"upstream_model": "second", "display_name": "Second"}}}, etagHeader(detail), 200)
	secondID := certify("second")
	assertAvailability(map[string]bool{"first": true, "second": false})
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	detail = h.want(owner, "PATCH", providerPath+"/models/"+firstID, map[string]any{"enabled": false}, etagHeader(detail), 200)
	assertAvailability(map[string]bool{"first": true, "second": false})
	slots := h.want(owner, "GET", providerPath+"/credential-slots", nil, nil, 200)
	slotID := slots["items"].([]any)[0].(map[string]any)["id"].(string)
	h.want(owner, "POST", providerPath+"/credential-slots/"+slotID+"/validate", nil, nil, 200)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "publish-second"}), 200)
	assertAvailability(map[string]bool{"first": false, "second": true})
	// Every contract surface filters the inventory; an openai-only model is
	// not available on another surface.
	for _, surface := range []string{"bedrock", "native"} {
		for _, item := range h.want(owner, "GET", "/api/v1/provider-models?surface="+surface, nil, nil, 200)["items"].([]any) {
			if item.(map[string]any)["available"] != false {
				t.Fatalf("openai-only model available on %s: %v", surface, item)
			}
		}
	}
	h.want(owner, "GET", "/api/v1/provider-models?surface=bogus", nil, nil, 400)
	// A route can keep serving the active model while an older provider
	// revision becomes the draft, even when their model sets differ.
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": routeSlug, "overall_timeout_ms": 5000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"},
		"targets": []any{map[string]any{"provider_model_id": secondID, "priority": 0, "weight": 1, "timeout_ms": 2000}},
	}, map[string]string{"Idempotency-Key": "draft"}, 201)
	draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draft), 200)
	route := h.want(owner, "POST", draftPath+"/activate", nil, withMatch(validated, map[string]string{"Idempotency-Key": "route"}), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "restore inference"}, map[string]string{"Idempotency-Key": "key"}, 201)
	h.refresh()
	sequence := h.Runtime.Release().Sequence
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	draftConfiguration := detail["configuration"].(map[string]any)
	draftConfiguration["options"].(map[string]any)["parameter_defaults"] = map[string]any{"temperature": 0.2}
	detail = h.want(owner, "PATCH", providerPath, map[string]any{"name": "Inventory publication", "configuration": draftConfiguration}, etagHeader(detail), 200)
	if detail["certified_capability_count"] != float64(0) {
		t.Fatal("transport change did not invalidate draft certification")
	}
	assertAvailability(map[string]bool{"first": false, "second": true})
	detail = h.want(owner, "POST", providerPath+"/discovery", map[string]any{"models": []any{map[string]any{"upstream_model": "draft-only"}}}, etagHeader(detail), 200)
	restored := h.want(owner, "POST", providerPath+"/revisions/1/restore-as-draft", nil, withMatch(detail, map[string]string{"Idempotency-Key": "restore-older"}), 200)
	detail = restored["provider"].(map[string]any)
	if restored["credential_restored"] != false || detail["pending_activation"] != true {
		t.Fatalf("older draft restore: %v", restored)
	}
	assertAvailability(map[string]bool{"first": false, "second": true})
	models := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)
	for _, raw := range models["items"].([]any) {
		model := raw.(map[string]any)
		first := model["upstream_model"] == "first"
		wantID := secondID
		if first {
			wantID = firstID
		}
		if model["id"] != wantID || model["enabled"] != first {
			t.Fatalf("restore changed a published identity or enabled the wrong model: %v", model)
		}
		if !first {
			for _, raw := range model["capabilities"].([]any) {
				if raw.(map[string]any)["source"] != "declared" {
					t.Fatalf("retained model kept evidence outside the restored revision: %v", model)
				}
			}
		}
	}
	live := h.want(owner, "GET", "/api/v1/routes/"+route["route_id"].(string), nil, nil, 200)
	if live["latest_revision"].(map[string]any)["targets"].([]any)[0].(map[string]any)["available"] != true {
		t.Fatalf("draft restore hid the published route target: %v", live)
	}
	currentDraft := h.want(owner, "GET", draftPath, nil, nil, 200)
	h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(currentDraft), 200)
	decisions := h.list(owner, "POST", "/api/v1/routing/simulate", map[string]any{"operation": map[string]any{"request": map[string]any{"route": routeSlug}}, "surface": "openai", "mode": "unary", "api_key_id": key["id"]}, nil, 200)
	if len(decisions) != 1 || decisions[0].(map[string]any)["eligible"] != true {
		t.Fatalf("restore changed routing simulation: %v", decisions)
	}
	h.refresh()
	status, result, _ := h.gateway("POST", "/v1/chat/completions", key["secret"].(string), map[string]any{"model": routeSlug, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if status != http.StatusOK || result["model"] != routeSlug || h.Runtime.Release().Sequence != sequence {
		t.Fatalf("draft restore changed serving state: %d %v", status, result)
	}
	detail = h.want(owner, "POST", providerPath+"/restore-as-draft", nil, withMatch(detail, map[string]string{"Idempotency-Key": "restore-active"}), 200)
	if detail["pending_activation"] != false {
		t.Fatalf("restoring the active revision did not produce a clean draft: %v", detail)
	}
	assertAvailability(map[string]bool{"first": false, "second": true})
	h.want(owner, "POST", providerPath+"/disable", nil, withMatch(detail, map[string]string{"Idempotency-Key": "disable"}), 200)
	assertAvailability(map[string]bool{"first": false, "second": false})
}
