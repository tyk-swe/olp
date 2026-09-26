//go:build integration

package integration_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestRoutingSimulationMatchesInferenceKeyAuthorization(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	up := newVendor(t)
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": "Key simulation", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"},
	}, map[string]string{"Idempotency-Key": "provider"}, 201)
	path := "/api/v1/providers/" + created["id"].(string)
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	certification := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(created), 200)
	if certification["status"] != "certified" {
		t.Fatalf("fixture certification: %v", certification)
	}
	detail := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "provider-activate"}), 200)
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": routeSlug, "overall_timeout_ms": 5000, "max_attempts": 1,
		"targets": []any{map[string]any{"provider_model_id": modelID, "priority": 0, "weight": 1, "timeout_ms": 2000}},
	}, map[string]string{"Idempotency-Key": "draft"}, 201)
	draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", draftPath+"/activate", nil, withMatch(validated, map[string]string{"Idempotency-Key": "route-activate"}), 200)
	for i, tc := range []struct {
		name, scope, route, state string
		status                    int
	}{
		{"inference", "inference", routeSlug, "", http.StatusOK},
		{"models only", "models_read", routeSlug, "", http.StatusForbidden},
		{"unrestricted", "inference", "", "", http.StatusOK},
		{"other route", "inference", "other-route", "", http.StatusForbidden},
		{"revoked", "inference", routeSlug, "revoked", http.StatusUnauthorized},
		{"expired", "inference", routeSlug, "expired", http.StatusUnauthorized},
		{"future expiry", "inference", routeSlug, "future", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allowedRoutes := []string{}
			if tc.route != "" {
				allowedRoutes = append(allowedRoutes, tc.route)
			}
			input := map[string]any{"name": tc.name, "scopes": []string{tc.scope}, "allowed_routes": allowedRoutes}
			if tc.state == "future" {
				input["expires_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			}
			key := h.want(owner, "POST", "/api/v1/api-keys", input, map[string]string{"Idempotency-Key": fmt.Sprintf("key-%d", i)}, 201)
			switch tc.state {
			case "revoked":
				keyPath := "/api/v1/api-keys/" + key["id"].(string)
				current := h.want(owner, "GET", keyPath, nil, nil, 200)
				h.want(owner, "POST", keyPath+"/revoke", nil, withMatch(current, map[string]string{"Idempotency-Key": "revoke"}), 200)
			case "expired":
				// Advance this fixture past expiry without sleeping or changing
				// the installation's clock.
				if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.api_keys SET expires_at=now()-interval '1 second' WHERE id=$1", key["id"]); err != nil {
					t.Fatal(err)
				}
			}
			h.refresh()
			calls := up.chats.Load()
			eligible := tc.status == http.StatusOK
			for _, mode := range []string{"unary", "streaming"} {
				decisions := h.list(owner, "POST", "/api/v1/routing/simulate", map[string]any{
					"operation": map[string]any{"operation": "generation", "request": map[string]any{"route": routeSlug}},
					"surface":   "openai", "mode": mode, "api_key_id": key["id"],
				}, nil, 200)
				if len(decisions) != 1 {
					t.Fatalf("missing decisions: %v", decisions)
				}
				decision := decisions[0].(map[string]any)
				var attempt, reason any
				if eligible {
					attempt = float64(1)
				} else {
					reason = "api_key_not_authorized"
					if tc.route == "other-route" {
						reason = "route_not_allowed_for_key"
					}
				}
				if decision["eligible"] != eligible || decision["attempt"] != attempt || decision["reason"] != reason {
					t.Fatalf("simulation disagrees with key authorization: %v", decision)
				}
			}
			if up.chats.Load() != calls {
				t.Fatal("simulation dispatched upstream")
			}
			status, result, _ := h.gateway("POST", "/v1/chat/completions", key["secret"].(string), map[string]any{"model": routeSlug, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
			if status != tc.status || (!eligible && up.chats.Load() != calls) {
				t.Fatalf("inference authorization: %d %v, want %d", status, result, tc.status)
			}
		})
	}
}
