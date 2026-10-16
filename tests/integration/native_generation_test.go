//go:build integration

package integration_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// provisionStrictNative activates a provider in a native generation dialect,
// certified for that dialect's unary and streaming tuples, behind a strict
// route.
func provisionStrictNative(t *testing.T, h *accessHarness, cfg map[string]any, model string) (string, string) {
	t.Helper()
	owner := h.owner()
	options := h.want(owner, "GET", "/api/v1/provider-kinds/openai_compatible/capabilities", nil, nil, 200)
	for _, mode := range []string{"unary", "streaming"} {
		matches := 0
		for _, item := range options["capabilities"].([]any) {
			option := item.(map[string]any)
			if option["operation"] == "generation" && option["surface"] == "native" && option["mode"] == mode {
				matches++
			}
		}
		if matches != 1 {
			t.Fatalf("expected one native generation/%s option, got %v", mode, options)
		}
	}
	detail := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Native " + cfg["profile_id"].(string), "configuration": cfg, "model": model, "credential": vendorSecret}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	path := "/api/v1/providers/" + detail["id"].(string)
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)
	var modelID string
	for _, item := range models["items"].([]any) {
		if row := item.(map[string]any); row["upstream_model"] == model {
			modelID = row["id"].(string)
		}
	}
	if modelID == "" {
		t.Fatalf("model %s was not declared: %v", model, models)
	}
	native := []any{map[string]any{"operation": "generation", "surface": "native", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "native", "mode": "streaming"}}
	detail = h.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": native}, etagHeader(detail), 200)
	certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200)
	if certified["status"] != "certified" {
		t.Fatalf("native certification: %v", certified)
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	slug := "native-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"slug": slug, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{}, "targets": []any{map[string]any{"provider_id": detail["id"], "provider_model": model, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	dialect := "mistral-fim"
	request := map[string]any{"model": slug, "prompt": "def f():"}
	if cfg["profile_id"] == "cohere-v2" {
		dialect = "cohere-chat-v2"
		request = map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	}
	for _, mode := range []string{"unary", "streaming"} {
		request["stream"] = mode == "streaming"
		for _, published := range []bool{false, true} {
			decision := inspectorSimulation(t, h, owner, draft, published, request, map[string]any{"dialect": dialect, "surface": "native", "mode": mode})
			if decision["eligible"] != true || decision["interaction"].(map[string]any)["class"] != "native_identity" {
				t.Fatalf("%s/%s published=%v: %v", dialect, mode, published, decision)
			}
		}
	}
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "native generation", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.refresh()
	return slug, key["secret"].(string)
}

func nativeStream(t *testing.T, h *accessHarness, path, secret, body string) (int, string) {
	t.Helper()
	status, reply, _ := h.gatewayRaw("POST", path, secret, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
	return status, string(reply)
}

// The route serves its native dialect only: a chat request finds no
// provider that speaks Chat Completions.
