//go:build integration

package integration_test

import (
	"fmt"
	"testing"
)

func TestRoutingSimulationsHonorAvailablePreferences(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	up := newVendor(t)
	var targets []any
	for i := range 2 {
		created := h.want(owner, "POST", "/api/v1/providers", map[string]any{
			"name": fmt.Sprintf("Simulation provider %d", i), "model": vendorModel, "credential": vendorSecret,
			"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"},
		}, map[string]string{"Idempotency-Key": fmt.Sprintf("provider-%d", i)}, 201)
		path := "/api/v1/providers/" + created["id"].(string)
		models := h.want(owner, "GET", path+"/models", nil, nil, 200)
		modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
		certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(created), 200)
		if certified["status"] != "certified" {
			t.Fatalf("fixture certification failed: %v", certified)
		}
		detail := h.want(owner, "GET", path, nil, nil, 200)
		h.want(owner, "POST", path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": fmt.Sprintf("activate-%d", i)}), 200)
		targets = append(targets, map[string]any{"provider_id": created["id"], "provider_model": vendorModel, "priority": i, "weight": 1, "timeout_ms": 2000})
	}
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"slug": routeSlug, "overall_timeout_ms": 5000, "max_attempts": 2, "fidelity": map[string]any{"mode": "transformed"}, "targets": targets}, map[string]string{"Idempotency-Key": "draft"}, 201)
	draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", draftPath+"/activate", nil, withMatch(validated, map[string]string{"Idempotency-Key": "route-activate"}), 200)
	input := func(path, mode string, preferences any) map[string]any {
		if path == "/api/v1/playground" {
			return map[string]any{"model": routeSlug, "input": "hi", "routing": preferences}
		}
		body := map[string]any{"operation": "generation", "surface": "openai", "mode": mode, "seed": "preferences", "preferences": preferences}
		if path == "/api/v1/routing/simulate" {
			body["operation"] = map[string]any{"operation": "generation", "request": map[string]any{"route": routeSlug}}
		}
		return body
	}
	for _, path := range []string{draftPath + "/simulate", "/api/v1/routing/simulate", "/api/v1/playground"} {
		t.Run(path, func(t *testing.T) {
			before := up.chats.Load()
			for _, preferences := range []map[string]any{
				{"strategy": "ordered"}, {"order": []string{"provider"}},
				{"ignore": []string{"provider"}}, {"only": []string{"provider"}},
				{"max_price": map[string]any{"input_per_million": "1e9"}},
			} {
				result := h.want(owner, "POST", path, input(path, "unary", preferences), nil, 422)
				if problemCode(t, result) != "validation_failed" {
					t.Fatalf("unsupported preference was not rejected: %v: %v", preferences, result)
				}
			}
			for _, preferences := range []any{true, []any{}, map[string]any{"require_zero_data_retention": "true"}, map[string]any{"unknown_preference": true}} {
				h.want(owner, "POST", path, input(path, "unary", preferences), nil, 400)
			}
			if path == "/api/v1/playground" {
				return
			}
			for _, mode := range []string{"unary", "streaming"} {
				for _, tc := range []struct {
					preferences any
					attempts    int
				}{
					{nil, 2}, {map[string]any{}, 2},
					{map[string]any{"strategy": "weighted", "allow_fallbacks": true}, 2},
					{map[string]any{"allow_fallbacks": false}, 1},
					{map[string]any{"strategy": "price"}, 2},
					{map[string]any{"strategy": "latency", "preferred_max_latency_ms": 0}, 2},
					{map[string]any{"strategy": "throughput", "preferred_min_throughput": 0}, 2},
					{map[string]any{"regions": []string{"eu"}}, 0},
					{map[string]any{"require_zero_data_retention": true}, 0},
					{map[string]any{"deny_data_collection": true}, 0},
					{map[string]any{"only": []any{}}, 0},
					{map[string]any{"order": []any{}, "ignore": []any{}, "only": nil, "max_price": nil, "regions": nil, "quantizations": nil, "require_parameters": false, "deny_data_collection": false, "require_zero_data_retention": false}, 2},
				} {
					var decisions []any
					if path == "/api/v1/routing/simulate" {
						decisions = h.list(owner, "POST", path, input(path, mode, tc.preferences), nil, 200)
					} else {
						result := h.want(owner, "POST", path, input(path, mode, tc.preferences), nil, 200)
						decisions = result["targets"].([]any)
					}
					if len(decisions) != 2 {
						t.Fatalf("missing simulation targets: %v", decisions)
					}
					for i, raw := range decisions {
						decision := raw.(map[string]any)
						eligible := i < tc.attempts
						var attempt, reason any
						if eligible {
							attempt = float64(i + 1)
						} else {
							reason = "attempt_budget_exhausted"
							if tc.attempts == 0 {
								reason = decision["reason"]
								if reason == nil {
									t.Fatal("constraint exclusion lacks a reason")
								}
							}
						}
						if decision["eligible"] != eligible || decision["attempt"] != attempt || decision["reason"] != reason {
							t.Fatalf("preferences %v were ignored in %s: %v", tc.preferences, mode, decision)
						}
					}
				}
			}
			if up.chats.Load() != before {
				t.Fatal("simulation dispatched upstream")
			}
		})
	}
}
