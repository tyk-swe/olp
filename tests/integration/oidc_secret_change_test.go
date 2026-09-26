//go:build integration && oidctest

package integration_test

import (
	"maps"
	"testing"
)

func TestOIDCSecretReplacementRequiresFreshOwnerSignIn(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	issuer := newTestIssuer(t)
	configuration := map[string]any{"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration", "client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true}
	saved := h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, nil, 200)
	claims := map[string]any{"sub": "owner-subject", "email": "owner@example.com"}
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_link"}, nil, 204)
	authorization := h.want(owner, "POST", "/api/v1/oidc/link", nil, nil, 200)["authorization_url"].(string)
	h.want(owner, "GET", issuer.callback(t, authorization, claims), nil, nil, 303)
	localSetting := h.want(owner, "GET", "/api/v1/settings/auth.local_login_enabled", nil, nil, 200)
	localSetting = h.want(owner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(localSetting), 200)

	// Resubmitting the same credential must preserve the verified owner path.
	saved = h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, etagHeader(saved), 200)
	pending := &browser{}
	authorization = h.want(pending, "POST", "/api/v1/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
	candidate := maps.Clone(configuration)
	candidate["client_secret"] = "mistyped-replacement-secret"
	problem := h.want(owner, "PUT", "/api/v1/oidc/configuration", candidate, etagHeader(saved), 409)
	if problem["type"] != "https://openllmproxy.dev/problems/last_usable_owner" {
		t.Fatal("secret replacement was not rejected by owner protection")
	}
	current := h.want(owner, "GET", "/api/v1/oidc/configuration", nil, nil, 200)
	if current["etag"] != saved["etag"] || current["has_client_secret"] != true {
		t.Fatal("rejected replacement changed the configuration")
	}
	var updates int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.audit WHERE action='oidc.configuration.update'").Scan(&updates); err != nil || updates != 2 {
		t.Fatalf("configuration update audits=%d, error=%v", updates, err)
	}
	expireSessions := func() {
		t.Helper()
		if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.sessions SET expires_at=now()-interval '1 second'"); err != nil {
			t.Fatal(err)
		}
	}
	expireSessions()
	h.want(pending, "GET", issuer.callback(t, authorization, claims), nil, nil, 303)
	owner = pending
	if profile := h.want(owner, "GET", "/api/v1/profile", nil, nil, 200); profile["role"] != "owner" {
		t.Fatal("rejected replacement did not preserve owner access")
	}

	// An independent local owner permits replacement, but stale evidence must
	// not allow local login to be disabled, even after another configuration save.
	localSetting = h.want(owner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "true"}, etagHeader(localSetting), 200)
	localOwner := &browser{}
	h.want(localOwner, "POST", "/api/v1/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, 201)
	saved = h.want(localOwner, "PUT", "/api/v1/oidc/configuration", candidate, etagHeader(saved), 200)
	saved = h.want(localOwner, "PUT", "/api/v1/oidc/configuration", candidate, etagHeader(saved), 200)
	delete(candidate, "client_secret")
	saved = h.want(localOwner, "PUT", "/api/v1/oidc/configuration", candidate, etagHeader(saved), 200)
	h.want(localOwner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(localSetting), 409)
	login := &browser{}
	authorization = h.want(login, "POST", "/api/v1/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
	problem = h.want(login, "GET", issuer.callback(t, authorization, claims), nil, nil, 403)
	if problem["type"] != "https://openllmproxy.dev/problems/oidc_token_invalid" {
		t.Fatal("the mistyped credential did not fail token exchange")
	}
	h.want(localOwner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(localSetting), 409)

	// Restoring working credentials still requires a successful owner sign-in.
	h.want(localOwner, "PUT", "/api/v1/oidc/configuration", configuration, etagHeader(saved), 200)
	h.want(localOwner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(localSetting), 409)
	authorization = h.want(login, "POST", "/api/v1/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
	h.want(login, "GET", issuer.callback(t, authorization, claims), nil, nil, 303)
	h.want(localOwner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(localSetting), 200)
	expireSessions()
	login = &browser{}
	authorization = h.want(login, "POST", "/api/v1/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
	h.want(login, "GET", issuer.callback(t, authorization, claims), nil, nil, 303)
	if profile := h.want(login, "GET", "/api/v1/profile", nil, nil, 200); profile["role"] != "owner" {
		t.Fatal("verified replacement did not preserve owner sign-in")
	}
}
