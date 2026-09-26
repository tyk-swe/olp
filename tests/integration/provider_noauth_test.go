//go:build integration

package integration_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestNoAuthPublicationIgnoresRetainedRevokedCredential(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	vendor := newVendor(t)
	var noAuth atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if noAuth.Load() {
			if r.Header.Get("Authorization") != "" {
				t.Error("no-auth provider sent an obsolete credential")
				http.Error(w, "unexpected credential", http.StatusUnauthorized)
				return
			}
			r.Header.Set("Authorization", "Bearer "+vendorSecret)
		}
		vendor.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)
	config := map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"}
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": "Switchable auth", "model": vendorModel, "credential": vendorSecret, "configuration": config,
	}, map[string]string{"Idempotency-Key": "provider"}, 201)
	providerID := created["id"].(string)
	providerPath := "/api/v1/providers/" + providerID
	models := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	certifyAndActivate := func(key string) {
		t.Helper()
		detail := h.want(owner, "GET", providerPath, nil, nil, 200)
		certification := h.want(owner, "POST", providerPath+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200)
		if certification["status"] != "certified" {
			t.Fatalf("certification failed: %v", certification)
		}
		detail = h.want(owner, "GET", providerPath, nil, nil, 200)
		h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": key}), 200)
	}
	certifyAndActivate("activate-authenticated")
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": routeSlug, "overall_timeout_ms": 5000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"},
		"targets": []any{map[string]any{"provider_id": providerID, "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 2000}},
	}, map[string]string{"Idempotency-Key": "route"}, 201)
	draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", draftPath+"/activate", nil, withMatch(validated, map[string]string{"Idempotency-Key": "activate-route"}), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "inference", "scopes": []string{"inference"}}, map[string]string{"Idempotency-Key": "key"}, 201)
	checkInference := func(wantStatus int) {
		t.Helper()
		calls := vendor.chats.Load()
		status, result, _ := h.gateway("POST", "/v1/chat/completions", key["secret"].(string), map[string]any{
			"model": routeSlug, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		if status != wantStatus {
			t.Fatalf("inference status %d, want %d: %v", status, wantStatus, result)
		}
		if wantStatus == http.StatusOK {
			if result["model"] != routeSlug || vendor.chats.Load() != calls+1 {
				t.Fatalf("successful inference did not use the provider: %v", result)
			}
		} else if h.gatewayCode(status, result) != "upstream_unavailable" || vendor.chats.Load() != calls {
			t.Fatalf("revoked authenticated credential remained usable: %v", result)
		}
	}
	h.refresh()
	checkInference(http.StatusOK)
	slots := h.want(owner, "GET", providerPath+"/credential-slots", nil, nil, 200)
	credentialID := slots["items"].([]any)[0].(map[string]any)["credential_version_id"].(string)
	detail := h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/credentials/"+credentialID+"/revoke", nil, withMatch(detail, map[string]string{"Idempotency-Key": "revoke"}), 200)
	h.refresh()
	checkInference(http.StatusServiceUnavailable)

	noAuth.Store(true)
	config["auth_mode"] = "none"
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "PATCH", providerPath, map[string]any{"name": "Switchable auth", "configuration": config}, etagHeader(detail), 200)
	// Changing the draft must not bypass revocation on the authenticated release.
	checkInference(http.StatusServiceUnavailable)
	certifyAndActivate("activate-no-auth")
	h.refresh()
	provider := h.Runtime.Release().Snapshot.Providers[providerID]
	if provider.AuthMode != "none" || provider.ActiveCredential != nil || len(provider.Slots) != 1 || !provider.Slots[0].Enabled || provider.Slots[0].CredentialID != nil || provider.Slots[0].CredentialVersion != nil {
		t.Fatalf("no-auth release retained obsolete credentials: %+v", provider)
	}
	checkInference(http.StatusOK)
	if text, done, status := h.stream(key["secret"].(string)); status != http.StatusOK || !done || text != vendorAnswer {
		t.Fatalf("no-auth stream failed: status=%d done=%t text=%q", status, done, text)
	}

	// Restoring the active no-auth revision compares slots using the restored
	// authentication mode, not the current authenticated draft.
	config["auth_mode"] = "api_key"
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	detail = h.want(owner, "PATCH", providerPath, map[string]any{"name": "Switchable auth", "configuration": config}, etagHeader(detail), 200)
	restored := h.want(owner, "POST", providerPath+"/restore-as-draft", nil, withMatch(detail, map[string]string{"Idempotency-Key": "restore-no-auth"}), 200)
	if restored["pending_activation"] != false {
		t.Fatalf("restored no-auth draft was incorrectly marked dirty: %v", restored)
	}
	slots = h.want(owner, "GET", providerPath+"/credential-slots", nil, nil, 200)
	if slots["items"].([]any)[0].(map[string]any)["credential_version_id"] != credentialID {
		t.Fatal("no-auth publication changed the retained draft credential")
	}
	checkInference(http.StatusOK)
}
