//go:build integration

package integration_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestCustomHeaderAuthenticationAndSimulationMatchLiveCredentialRestrictions(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	vendor := newVendor(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "header-secret" || r.Header.Get("X-Tenant") != "tenant-secret" || r.Header.Get("Authorization") != "" {
			http.Error(w, "invalid custom headers", http.StatusUnauthorized)
			return
		}
		r.Header.Set("Authorization", "Bearer "+vendorSecret)
		vendor.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": "Custom headers", "model": vendorModel,
		"credential":    `{"x-api-key":"header-secret","X-TENANT":"tenant-secret"}`,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "headers", "endpoint": up.URL + "/v1", "options": map[string]any{"credential_headers": []string{"x-api-key", "X-Tenant"}}},
	}, map[string]string{"Idempotency-Key": "provider"}, 201)
	providerPath := "/api/v1/providers/" + created["id"].(string)
	probe := h.want(owner, "POST", providerPath+"/probe", nil, etagHeader(created), 200)
	if probe["succeeded"] != true {
		t.Fatalf("custom-header probe failed: %v", probe)
	}
	models := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	detail := h.want(owner, "GET", providerPath, nil, nil, 200)
	certification := h.want(owner, "POST", providerPath+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200)
	if certification["status"] != "certified" {
		t.Fatalf("custom-header certification failed: %v", certification)
	}
	activateProvider := func(key string) {
		detail := h.want(owner, "GET", providerPath, nil, nil, 200)
		h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": key}), 200)
	}
	activateProvider("activate-initial")
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": routeSlug, "overall_timeout_ms": 5000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"},
		"targets": []any{map[string]any{"provider_id": created["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 2000}},
	}, map[string]string{"Idempotency-Key": "draft"}, 201)
	draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", draftPath+"/activate", nil, withMatch(validated, map[string]string{"Idempotency-Key": "route-activate"}), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "inference", "scopes": []string{"inference"}, "allowed_routes": []string{routeSlug}}, map[string]string{"Idempotency-Key": "key"}, 201)
	h.refresh()
	checkDecision := func(decision map[string]any, eligible bool) {
		t.Helper()
		var attempt, reason any
		if eligible {
			attempt = float64(1)
		} else {
			reason = "no_eligible_credentials"
		}
		if decision["eligible"] != eligible || decision["attempt"] != attempt || decision["reason"] != reason {
			t.Fatalf("simulation eligibility=%t: %v", eligible, decision)
		}
	}
	check := func(eligible, anonymous bool) {
		t.Helper()
		for _, mode := range []string{"unary", "streaming"} {
			draftSimulation := h.want(owner, "POST", draftPath+"/simulate", map[string]any{"operation": "generation", "surface": "openai", "mode": mode, "seed": "test"}, nil, 200)
			checkDecision(draftSimulation["targets"].([]any)[0].(map[string]any), anonymous)
			for _, withKey := range []bool{false, true} {
				input := map[string]any{"operation": map[string]any{"operation": "generation", "request": map[string]any{"route": routeSlug}}, "surface": "openai", "mode": mode, "seed": "test"}
				want := anonymous
				if withKey {
					input["api_key_id"], want = key["id"], eligible
				}
				decisions := h.list(owner, "POST", "/api/v1/routing/simulate", input, nil, 200)
				checkDecision(decisions[0].(map[string]any), want)
			}
		}
		calls := vendor.chats.Load()
		status, result, _ := h.gateway("POST", "/v1/chat/completions", key["secret"].(string), map[string]any{"model": routeSlug, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
		if eligible {
			if status != http.StatusOK || result["model"] != routeSlug || vendor.chats.Load() != calls+1 {
				t.Fatalf("eligible inference: %d %v", status, result)
			}
		} else if status != http.StatusServiceUnavailable || h.gatewayCode(status, result) != "upstream_unavailable" || vendor.chats.Load() != calls {
			t.Fatalf("ineligible inference: %d %v", status, result)
		}
	}
	check(true, true)
	previousEligible, previousAnonymous := true, true
	for i, tc := range []struct {
		name                string
		restrictions        map[string]any
		eligible, anonymous bool
	}{
		{"route excluded", map[string]any{"allowed_routes": []string{"other-route"}}, false, false},
		{"route allowed", map[string]any{"allowed_routes": []string{routeSlug}}, true, true},
		{"model excluded", map[string]any{"allowed_models": []string{"other-model"}}, false, false},
		{"model allowed", map[string]any{"allowed_models": []string{vendorModel}}, true, true},
		{"key excluded", map[string]any{"allowed_api_keys": []string{uuid.NewString()}}, false, false},
		{"key allowed", map[string]any{"allowed_api_keys": []string{key["id"].(string)}}, true, false},
		{"unrestricted", map[string]any{}, true, true},
	} {
		t.Log(tc.name)
		listPath := providerPath + "/credential-slots"
		slots := h.want(owner, "GET", listPath, nil, nil, 200)
		slotID := slots["items"].([]any)[0].(map[string]any)["id"].(string)
		input := map[string]any{"name": "default", "enabled": true}
		for name, value := range tc.restrictions {
			input[name] = value
		}
		h.want(owner, "PUT", listPath+"/"+slotID, map[string]any{"slot": input}, withMatch(slots, map[string]string{"Idempotency-Key": fmt.Sprintf("slot-%d", i)}), 200)
		// Draft slot edits must not change either simulator's serving view.
		check(previousEligible, previousAnonymous)
		activateProvider(fmt.Sprintf("activate-%d", i))
		h.refresh()
		check(tc.eligible, tc.anonymous)
		previousEligible, previousAnonymous = tc.eligible, tc.anonymous
	}
	// Revocation applies immediately, without publishing another revision.
	slots := h.want(owner, "GET", providerPath+"/credential-slots", nil, nil, 200)
	credentialID := slots["items"].([]any)[0].(map[string]any)["credential_version_id"].(string)
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/credentials/"+credentialID+"/revoke", nil, withMatch(detail, map[string]string{"Idempotency-Key": "revoke"}), 200)
	h.refresh()
	check(false, false)
}
