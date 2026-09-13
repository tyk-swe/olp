//go:build integration && oidctest

package integration_test

import (
	"maps"
	"testing"
)

func TestConcurrentOIDCConsumptionAndIdentityLinking(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	member := h.invite(owner, "linker@example.com", "developer")
	issuer := newTestIssuer(t)
	h.want(owner, "PUT", "/api/v3/oidc/configuration", map[string]any{"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration", "client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true, "default_role": "viewer"}, nil, 200)
	login := &browser{}
	authorization := h.want(login, "POST", "/api/v3/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
	callback := issuer.callback(t, authorization, nil)
	start := make(chan struct{})
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		copy := &browser{Cookies: maps.Clone(login.Cookies), CSRF: login.CSRF}
		go func() { <-start; status, _, _ := h.request(copy, "GET", callback, nil, nil); results <- status }()
	}
	close(start)
	a, b := <-results, <-results
	if !((a == 303 && b == 403) || (a == 403 && b == 303)) {
		t.Fatalf("callback race: %d, %d", a, b)
	}
	start = make(chan struct{})
	for _, browser := range []*browser{owner, member} {
		h.want(browser, "POST", "/api/v3/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_link"}, nil, 204)
		authorization := h.want(browser, "POST", "/api/v3/oidc/link", nil, nil, 200)["authorization_url"].(string)
		callback := issuer.callback(t, authorization, map[string]any{"sub": "contested-identity", "email": "contested@example.com"})
		go func() { <-start; status, _, _ := h.request(browser, "GET", callback, nil, nil); results <- status }()
	}
	close(start)
	a, b = <-results, <-results
	if !((a == 303 && b == 409) || (a == 409 && b == 303)) {
		t.Fatalf("identity link race: %d, %d", a, b)
	}
	var count int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp_go.oidc_identities WHERE subject='contested-identity'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("linked identity count=%d", count)
	}
}

func TestOIDCRoleSyncRetiresSessionsAndOutstandingGrants(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	issuer := newTestIssuer(t)
	configuration := map[string]any{"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration", "client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true, "default_role": "owner"}
	saved := h.want(owner, "PUT", "/api/v3/oidc/configuration", configuration, nil, 200)
	login := func(browser *browser, status int) {
		authorization := h.want(browser, "POST", "/api/v3/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
		h.want(browser, "GET", issuer.callback(t, authorization, nil), nil, nil, status)
	}
	old := &browser{}
	login(old, 303)
	pending := h.want(old, "POST", "/api/v3/invitations", map[string]any{"email": "outstanding@example.com", "role": "owner"}, map[string]string{"Idempotency-Key": "outstanding"}, 201)
	configuration["default_role"] = "developer"
	saved = h.want(owner, "PUT", "/api/v3/oidc/configuration", configuration, etagHeader(saved), 200)
	current := &browser{}
	login(current, 303)
	profile := h.want(current, "GET", "/api/v3/profile", nil, nil, 200)
	if profile["role"] != "developer" {
		t.Fatal("OIDC-only role did not follow current mapping")
	}
	h.want(old, "GET", "/api/v3/sessions/current", nil, nil, 401)
	h.want(nil, "POST", "/api/v3/invitations/accept", map[string]any{"token": pending["token"], "display_name": "Pending", "password": accessPassword}, nil, 410)
	var unattributed bool
	if err := h.Pool.QueryRow(t.Context(), "SELECT actor_user_id IS NULL FROM olp_go.audit WHERE action='user.role_sync_oidc'").Scan(&unattributed); err != nil || !unattributed {
		t.Fatal("automatic role sync must not invent a human actor")
	}
	configuration["default_role"] = nil
	h.want(owner, "PUT", "/api/v3/oidc/configuration", configuration, etagHeader(saved), 200)
	login(&browser{}, 403)
}
