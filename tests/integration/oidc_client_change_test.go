//go:build integration && oidctest

package integration_test

import (
	"maps"
	"testing"
)

func TestOIDCClientChangeRequiresAnIndependentOwnerSignIn(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	profile := h.want(owner, "GET", "/api/v1/profile", nil, nil, 200)
	issuer := newTestIssuer(t)
	configuration := map[string]any{"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration", "client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true}
	saved := h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, nil, 200)
	claims := map[string]any{"sub": "original-owner-subject", "email": "owner@example.com"}
	link := func(b *browser, claims map[string]any) {
		t.Helper()
		h.want(b, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_link"}, nil, 204)
		authorization := h.want(b, "POST", "/api/v1/oidc/link", nil, nil, 200)["authorization_url"].(string)
		h.want(b, "GET", issuer.callback(t, authorization, claims), nil, nil, 303)
	}
	expireSessions := func() {
		t.Helper()
		if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.sessions SET expires_at=now()-interval '1 second'"); err != nil {
			t.Fatal(err)
		}
	}
	link(owner, claims)
	localSetting := h.want(owner, "GET", "/api/v1/settings/auth.local_login_enabled", nil, nil, 200)
	localSetting = h.want(owner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(localSetting), 200)

	// A stored password is not an independent path while local login is disabled.
	pending := &browser{}
	authorization := h.want(pending, "POST", "/api/v1/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
	candidate := maps.Clone(configuration)
	candidate["client_id"] = "replacement-client"
	candidate["client_secret"] = "must-not-survive-the-rejected-update"
	problem := h.want(owner, "PUT", "/api/v1/oidc/configuration", candidate, etagHeader(saved), 409)
	if problem["type"] != "https://openllmproxy.dev/problems/last_usable_owner" {
		t.Fatal("the client change was not rejected by owner protection")
	}
	current := h.want(owner, "GET", "/api/v1/oidc/configuration", nil, nil, 200)
	if current["etag"] != saved["etag"] || current["client_id"] != "test-client" {
		t.Fatal("a rejected client change altered the configuration")
	}
	var updates int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp_go.audit WHERE action='oidc.configuration.update'").Scan(&updates); err != nil || updates != 1 {
		t.Fatalf("configuration update audits=%d, error=%v", updates, err)
	}
	expireSessions()
	h.want(pending, "GET", issuer.callback(t, authorization, claims), nil, nil, 303)
	owner = pending
	if current := h.want(owner, "GET", "/api/v1/profile", nil, nil, 200); current["id"] != profile["id"] || current["role"] != "owner" {
		t.Fatal("the original client could not restore owner access")
	}

	// Enabling and exercising the local path permits a deliberate client change.
	localSetting = h.want(owner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "true"}, etagHeader(localSetting), 200)
	localOwner := &browser{}
	h.want(localOwner, "POST", "/api/v1/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, 201)
	candidate["client_secret"] = configuration["client_secret"]
	h.want(localOwner, "PUT", "/api/v1/oidc/configuration", candidate, etagHeader(saved), 200)
	h.want(localOwner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(localSetting), 409)

	// A pairwise provider returns a new subject for the same person on the new client.
	claims = map[string]any{"sub": "replacement-owner-subject", "email": "owner@example.com"}
	login := &browser{}
	authorization = h.want(login, "POST", "/api/v1/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
	problem = h.want(login, "GET", issuer.callback(t, authorization, claims), nil, nil, 409)
	if problem["type"] != "https://openllmproxy.dev/problems/oidc_link_required" {
		t.Fatal("the replacement subject must require explicit linking")
	}
	link(localOwner, claims)
	h.want(localOwner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(localSetting), 200)
	expireSessions()
	login = &browser{}
	authorization = h.want(login, "POST", "/api/v1/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
	h.want(login, "GET", issuer.callback(t, authorization, claims), nil, nil, 303)
	if current := h.want(login, "GET", "/api/v1/profile", nil, nil, 200); current["id"] != profile["id"] || current["role"] != "owner" {
		t.Fatal("the replacement client could not restore owner access after linking")
	}
}
