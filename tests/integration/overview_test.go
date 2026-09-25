//go:build integration

package integration_test

import (
	"net/http"
	"testing"
	"time"
)

// Overview answers the console's setup-readiness aggregates in one round trip.
// Lifecycle transitions verify that the counts read current state and preserve
// setup readiness when a published provider is disabled.
func TestOverviewCountsReadinessAggregates(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()

	h.want(nil, http.MethodGet, "/api/v3/overview", nil, nil, 401)

	want := func(body map[string]any, providers, routes, models float64, key bool) {
		t.Helper()
		if body["active_providers"] != providers || body["active_routes"] != routes ||
			body["enabled_models"] != models || body["usable_api_key"] != key {
			t.Fatalf("want %v providers=%v routes=%v models=%v key=%v",
				body, providers, routes, models, key)
		}
	}
	overview := func() map[string]any {
		return h.want(owner, http.MethodGet, "/api/v3/overview", nil, nil, 200)
	}

	want(overview(), 0, 0, 0, false)

	// A draft provider's seed model counts toward enabled_models; the provider
	// itself stays inactive until activation.
	up := newVendor(t)
	created := h.want(owner, http.MethodPost, "/api/v3/providers", map[string]any{
		"name": "Overview vendor", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key",
			"endpoint": up.URL + "/v1"},
	}, map[string]string{"Idempotency-Key": "provider"}, 201)
	provider := created["id"].(string)
	providerPath := "/api/v3/providers/" + provider
	want(overview(), 0, 0, 1, false)

	if probe := h.want(owner, http.MethodPost, providerPath+"/probe", nil, etagHeader(created), 200); probe["succeeded"] != true {
		t.Fatalf("probe failed: %v", probe)
	}
	models := h.want(owner, http.MethodGet, providerPath+"/models", nil, nil, 200)
	model := models["items"].([]any)[0].(map[string]any)["id"].(string)
	detail := h.want(owner, http.MethodGet, providerPath, nil, nil, 200)
	if certified := h.want(owner, http.MethodPost, providerPath+"/models/"+model+"/certify", nil, etagHeader(detail), 200); certified["status"] != "certified" {
		t.Fatalf("certification failed: %v", certified)
	}
	detail = h.want(owner, http.MethodGet, providerPath, nil, nil, 200)
	h.want(owner, http.MethodPost, providerPath+"/activate", nil,
		withMatch(detail, map[string]string{"Idempotency-Key": "activate"}), 200)
	want(overview(), 1, 0, 1, false)

	draft := h.want(owner, http.MethodPost, "/api/v3/route-drafts", map[string]any{
		"slug": "overview-chat", "overall_timeout_ms": 20000, "max_attempts": 1,
		"targets": []any{map[string]any{"provider_id": provider, "provider_model": vendorModel,
			"priority": 0, "weight": 1, "timeout_ms": 15000}},
	}, map[string]string{"Idempotency-Key": "draft"}, 201)
	draftPath := "/api/v3/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, http.MethodPost, draftPath+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, http.MethodPost, draftPath+"/activate", nil,
		withMatch(validated, map[string]string{"Idempotency-Key": "route-activate"}), 200)
	want(overview(), 1, 1, 1, false)

	// Setup counts non-revoked keys, including expired keys. This flag does
	// not grant inference authority; revocation alone clears the setup step.
	key := h.want(owner, http.MethodPost, "/api/v3/api-keys", map[string]any{
		"name": "Overview key", "scopes": []string{"inference"}, "allowed_routes": []string{"overview-chat"},
	}, map[string]string{"Idempotency-Key": "key"}, 201)
	want(overview(), 1, 1, 1, true)
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.api_keys SET expires_at=$2 WHERE id=$1", key["id"], time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	want(overview(), 1, 1, 1, true)
	authority, err := h.authority(key["secret"].(string))
	if err != nil || authority.Allows("inference", "overview-chat", nil, time.Now()) {
		t.Fatal("setup completion must not authorize an expired key", err)
	}
	keyRecord := h.want(owner, http.MethodGet, "/api/v3/api-keys/"+key["id"].(string), nil, nil, 200)
	h.want(owner, http.MethodPost, "/api/v3/api-keys/"+key["id"].(string)+"/revoke", nil,
		withMatch(keyRecord, map[string]string{"Idempotency-Key": "revoke"}), 200)
	want(overview(), 1, 1, 1, false)

	// Disabling the only published provider retains its active revision, so the
	// completed provider setup step must remain complete.
	detail = h.want(owner, http.MethodGet, providerPath, nil, nil, 200)
	h.want(owner, http.MethodPost, providerPath+"/disable", nil,
		withMatch(detail, map[string]string{"Idempotency-Key": "disable"}), 200)
	disabled := h.want(owner, http.MethodGet, providerPath, nil, nil, 200)
	if disabled["state"] != "disabled" || disabled["active_revision"] == nil {
		t.Fatalf("disabled provider lost its published revision: %v", disabled)
	}
	want(overview(), 1, 1, 1, false)
}
