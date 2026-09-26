//go:build integration && oidctest

package integration_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOIDCConfigurationCanDisableUnavailableProvider(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	viewer := h.invite(owner, "viewer@example.com", "viewer")
	issuer := newTestIssuer(t)
	configuration := map[string]any{
		"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration",
		"client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true,
	}
	saved := h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, nil, 200)
	issuer.Server.Close()
	delete(configuration, "client_secret")
	configuration["enabled"] = false
	h.want(viewer, "PUT", "/api/v1/oidc/configuration", configuration, etagHeader(saved), 403)
	h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, nil, 428)
	h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, etagHeader(map[string]any{"etag": "stale"}), 412)
	current := h.want(owner, "GET", "/api/v1/oidc/configuration", nil, nil, 200)
	if current["etag"] != saved["etag"] || current["enabled"] != true {
		t.Fatal("a rejected disable changed the configuration")
	}
	disabled := h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, etagHeader(saved), 200)
	current = h.want(owner, "GET", "/api/v1/oidc/configuration", nil, nil, 200)
	if current["enabled"] != false || current["etag"] != disabled["etag"] || current["etag"] == saved["etag"] || current["has_client_secret"] != true {
		t.Fatal("disabling did not persist the configuration and preserve its secret")
	}
	h.want(nil, "POST", "/api/v1/oidc/login", map[string]any{}, nil, 403)
	for _, enabled := range []any{true, nil} {
		configuration["enabled"] = enabled
		if enabled == nil {
			delete(configuration, "enabled")
		}
		h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, etagHeader(disabled), 422)
	}
	current = h.want(owner, "GET", "/api/v1/oidc/configuration", nil, nil, 200)
	if current["enabled"] != false || current["etag"] != disabled["etag"] {
		t.Fatal("failed discovery changed the disabled configuration")
	}
}

func TestOIDCDisableUnavailableProviderPreservesLastUsableOwner(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	issuer := newTestIssuer(t)
	configuration := map[string]any{
		"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration",
		"client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true,
	}
	saved := h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, nil, 200)
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_link"}, nil, 204)
	authorization := h.want(owner, "POST", "/api/v1/oidc/link", nil, nil, 200)["authorization_url"].(string)
	h.want(owner, "GET", issuer.callback(t, authorization, map[string]any{"sub": "owner-subject", "email": "owner@example.com"}), nil, nil, 303)
	local := h.want(owner, "GET", "/api/v1/settings/auth.local_login_enabled", nil, nil, 200)
	h.want(owner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(local), 200)
	issuer.Server.Close()
	configuration["enabled"] = false
	problem := h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, etagHeader(saved), 409)
	if problem["type"] != "https://openllmproxy.dev/problems/last_usable_owner" {
		t.Fatal("disabling the last owner sign-in path was not rejected by owner protection")
	}
	current := h.want(owner, "GET", "/api/v1/oidc/configuration", nil, nil, 200)
	if current["enabled"] != true || current["etag"] != saved["etag"] {
		t.Fatal("a rejected disable changed the configuration")
	}
}

func TestOIDCConfigurationRejectsPrivateAdvertisedEndpoints(t *testing.T) {
	for _, field := range []string{"issuer", "authorization_endpoint", "token_endpoint", "jwks_uri"} {
		t.Run(field, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			const privateEndpoint = "https://10.0.0.1/endpoint"
			var issuer *httptest.Server
			issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				metadata := map[string]string{
					"issuer": issuer.URL, "authorization_endpoint": issuer.URL + "/authorize",
					"token_endpoint": issuer.URL + "/token", "jwks_uri": issuer.URL + "/jwks",
				}
				if r.URL.Path == "/unsafe" {
					metadata[field] = privateEndpoint
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(metadata)
			}))
			t.Cleanup(issuer.Close)
			valid := map[string]any{
				"issuer": issuer.URL, "discovery_url": issuer.URL + "/safe",
				"client_id": "test-client", "client_secret": "write-only-client-secret",
			}
			unsafe := maps.Clone(valid)
			unsafe["discovery_url"] = issuer.URL + "/unsafe"
			if field == "issuer" {
				unsafe["issuer"] = privateEndpoint
			}
			rejected := h.want(owner, "PUT", "/api/v1/oidc/configuration", unsafe, nil, 422)
			if rejected["errors"].(map[string]any)["discovery_url"] == nil {
				t.Fatal("unsafe advertised endpoint must be a discovery validation error")
			}
			h.want(owner, "GET", "/api/v1/oidc/configuration", nil, nil, 404)
			saved := h.want(owner, "PUT", "/api/v1/oidc/configuration", valid, nil, 200)
			h.want(owner, "PUT", "/api/v1/oidc/configuration", unsafe, etagHeader(saved), 422)
			current := h.want(owner, "GET", "/api/v1/oidc/configuration", nil, nil, 200)
			if current["etag"] != saved["etag"] || current["discovery_url"] != saved["discovery_url"] || current["issuer"] != saved["issuer"] {
				t.Fatal("a rejected endpoint changed the saved configuration")
			}
		})
	}
}
