//go:build integration && oidctest

package integration_test

import (
	"maps"
	"slices"
	"testing"
)

func TestOIDCScopeReductionRequiresAnIndependentOwnerSignIn(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	issuer := newTestIssuer(t)
	configuration := map[string]any{"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration", "client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true}
	saved := h.want(owner, "PUT", "/api/v3/oidc/configuration", configuration, nil, 200)
	claims := map[string]any{"sub": "owner-subject", "email": "owner@example.com"}
	h.want(owner, "POST", "/api/v3/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_link"}, nil, 204)
	authorization := h.want(owner, "POST", "/api/v3/oidc/link", nil, nil, 200)["authorization_url"].(string)
	h.want(owner, "GET", issuer.callback(t, authorization, claims), nil, nil, 303)
	localSetting := h.want(owner, "GET", "/api/v3/settings/auth.local_login_enabled", nil, nil, 200)
	localSetting = h.want(owner, "PUT", "/api/v3/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(localSetting), 200)

	// A stored password cannot protect the owner while local login is disabled.
	pending := &browser{}
	authorization = h.want(pending, "POST", "/api/v3/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
	candidate := maps.Clone(configuration)
	candidate["scopes"] = []string{"openid"}
	candidate["client_secret"] = "must-not-survive-the-rejected-update"
	problem := h.want(owner, "PUT", "/api/v3/oidc/configuration", candidate, etagHeader(saved), 409)
	if problem["type"] != "https://openllmproxy.dev/problems/last_usable_owner" {
		t.Fatal("the scope reduction was not rejected by owner protection")
	}
	current := h.want(owner, "GET", "/api/v3/oidc/configuration", nil, nil, 200)
	if current["etag"] != saved["etag"] || !slices.Equal(current["scopes"].([]any), saved["scopes"].([]any)) {
		t.Fatal("a rejected scope reduction changed the configuration")
	}
	var updates int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp_go.audit WHERE action='oidc.configuration.update'").Scan(&updates); err != nil || updates != 1 {
		t.Fatalf("configuration update audits=%d, error=%v", updates, err)
	}
	expireSessions := func() {
		t.Helper()
		if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.sessions SET expires_at=now()-interval '1 second'"); err != nil {
			t.Fatal(err)
		}
	}
	expireSessions()
	h.want(pending, "GET", issuer.callback(t, authorization, claims), nil, nil, 303)
	owner = pending
	if profile := h.want(owner, "GET", "/api/v3/profile", nil, nil, 200); profile["role"] != "owner" {
		t.Fatal("the preserved flow could not restore owner authority")
	}

	// Local login permits the change, but must remain enabled until OIDC is verified again.
	localSetting = h.want(owner, "PUT", "/api/v3/settings/auth.local_login_enabled", map[string]any{"value": "true"}, etagHeader(localSetting), 200)
	localOwner := &browser{}
	h.want(localOwner, "POST", "/api/v3/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, 201)
	candidate["client_secret"] = configuration["client_secret"]
	h.want(localOwner, "PUT", "/api/v3/oidc/configuration", candidate, etagHeader(saved), 200)
	h.want(localOwner, "PUT", "/api/v3/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(localSetting), 409)
	login := &browser{}
	authorization = h.want(login, "POST", "/api/v3/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
	withoutEmail := maps.Clone(claims)
	withoutEmail["email"], withoutEmail["email_verified"] = nil, nil
	problem = h.want(login, "GET", issuer.callback(t, authorization, withoutEmail), nil, nil, 403)
	if problem["type"] != "https://openllmproxy.dev/problems/oidc_email_unverified" {
		t.Fatal("login without a verified email was not rejected")
	}
	h.want(localOwner, "PUT", "/api/v3/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(localSetting), 409)

	// A successful login with verified claims at the reduced scopes refreshes the evidence.
	authorization = h.want(login, "POST", "/api/v3/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
	h.want(login, "GET", issuer.callback(t, authorization, claims), nil, nil, 303)
	h.want(localOwner, "PUT", "/api/v3/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(localSetting), 200)
	expireSessions()
	login = &browser{}
	authorization = h.want(login, "POST", "/api/v3/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
	h.want(login, "GET", issuer.callback(t, authorization, claims), nil, nil, 303)
	if profile := h.want(login, "GET", "/api/v3/profile", nil, nil, 200); profile["role"] != "owner" {
		t.Fatal("freshly verified scopes did not preserve owner sign-in")
	}
}
