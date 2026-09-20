//go:build integration && oidctest

package integration_test

import (
	"maps"
	"net/url"
	"strings"
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

func TestOIDCManagedPasswordCannotEscapeVerifiedDeauthorization(t *testing.T) {
	h := newAccessHarness(t)
	bootstrap := h.owner()
	issuer := newTestIssuer(t)
	h.want(bootstrap, "PUT", "/api/v3/oidc/configuration", map[string]any{"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration", "client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true, "group_role_mappings": []any{map[string]string{"claim_value": "owners", "role": "owner"}, map[string]string{"claim_value": "readers", "role": "viewer"}}}, nil, 200)
	claims := map[string]any{"groups": []string{"owners"}}
	authorize := func(b *browser, path string, input any, claims map[string]any, status int) {
		t.Helper()
		authorization := h.want(b, "POST", path, input, nil, 200)["authorization_url"].(string)
		h.want(b, "GET", issuer.callback(t, authorization, claims), nil, nil, status)
	}
	member := &browser{}
	authorize(member, "/api/v3/oidc/login", map[string]any{}, claims, 303)
	profile := h.want(member, "GET", "/api/v3/profile", nil, nil, 200)
	authorize(member, "/api/v3/oidc/reauthenticate", map[string]any{"purpose": "password_enrollment"}, claims, 303)
	h.want(member, "POST", "/api/v3/profile/password/enroll", map[string]any{"new_password": accessPassword}, etagHeader(profile), 200)
	var management string
	if err := h.Pool.QueryRow(t.Context(), "SELECT role_management FROM olp_go.users WHERE id=$1", profile["id"]).Scan(&management); err != nil || management != "oidc" {
		t.Fatalf("ownership after enrollment=%s, err=%v", management, err)
	}
	identity := h.want(member, "GET", "/api/v3/oidc/identities", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	if identity["can_unlink"] != false {
		t.Fatal("password enrollment must not sever the external authorization source")
	}
	h.want(member, "POST", "/api/v3/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_unlink", "resource_id": identity["id"]}, nil, 204)
	h.want(member, "DELETE", "/api/v3/oidc/identities/"+identity["id"].(string), nil, nil, 409)
	local := &browser{}
	h.want(local, "POST", "/api/v3/sessions", map[string]any{"email": "oidc@example.com", "password": accessPassword}, nil, 201)
	// Administrative guards still apply, but a verified external loss of the
	// final owner's groups must not preserve authority.
	bootstrapProfile := h.want(bootstrap, "GET", "/api/v3/profile", nil, nil, 200)
	h.want(member, "PATCH", "/api/v3/users/"+bootstrapProfile["id"].(string), map[string]any{"active": false}, etagHeader(bootstrapProfile), 200)
	invitation := h.want(member, "POST", "/api/v3/invitations", map[string]any{"email": "pending-denied@example.com", "role": "owner"}, map[string]string{"Idempotency-Key": "denied-invitation"}, 201)
	h.want(member, "POST", "/api/v3/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_link"}, nil, 204)
	var sequence int64
	if err := h.Pool.QueryRow(t.Context(), "SELECT authority_sequence FROM olp_go.installation").Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	// An unverified token must not revoke anything.
	authorize(&browser{}, "/api/v3/oidc/login", map[string]any{}, map[string]any{"nonce": "incorrect", "groups": []string{}}, 403)
	h.want(member, "GET", "/api/v3/sessions/current", nil, nil, 200)
	// Distinct verified callbacks race to deny the same external identity.
	start := make(chan struct{})
	results := make(chan int, 2)
	for range 2 {
		b := &browser{}
		authorization := h.want(b, "POST", "/api/v3/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
		callback := issuer.callback(t, authorization, map[string]any{"groups": []string{}})
		go func() { <-start; status, _, _ := h.request(b, "GET", callback, nil, nil); results <- status }()
	}
	close(start)
	for range 2 {
		if status := <-results; status != 403 {
			t.Fatalf("denial=%d", status)
		}
	}
	h.want(member, "GET", "/api/v3/sessions/current", nil, nil, 401)
	h.want(local, "GET", "/api/v3/profile", nil, nil, 401)
	h.want(nil, "POST", "/api/v3/sessions", map[string]any{"email": "oidc@example.com", "password": accessPassword}, nil, 401)
	h.want(nil, "POST", "/api/v3/invitations/accept", map[string]any{"token": invitation["token"], "display_name": "Denied", "password": accessPassword}, nil, 410)
	var authorized bool
	var grants int
	var after int64
	if err := h.Pool.QueryRow(t.Context(), "SELECT oidc_authorized,(SELECT count(*) FROM olp_go.recent_auth),(SELECT authority_sequence FROM olp_go.installation) FROM olp_go.users WHERE id=$1", profile["id"]).Scan(&authorized, &grants, &after); err != nil || authorized || grants != 0 || after != sequence+1 {
		t.Fatalf("revocation: authorized=%v grants=%d sequence=%d->%d err=%v", authorized, grants, sequence, after, err)
	}
	// Recovery synchronizes the lower mapping, without resurrecting old sessions
	// or outstanding invitations, and permits the enrolled password again.
	recovered := &browser{}
	authorize(recovered, "/api/v3/oidc/login", map[string]any{}, map[string]any{"groups": []string{"readers"}}, 303)
	if got := h.want(recovered, "GET", "/api/v3/profile", nil, nil, 200)["role"]; got != "viewer" {
		t.Fatalf("recovered role=%v", got)
	}
	h.want(local, "GET", "/api/v3/sessions/current", nil, nil, 401)
	passwordSession := &browser{}
	h.want(passwordSession, "POST", "/api/v3/sessions", map[string]any{"email": "oidc@example.com", "password": accessPassword}, nil, 201)
	if got := h.want(passwordSession, "GET", "/api/v3/profile", nil, nil, 200)["role"]; got != "viewer" {
		t.Fatalf("password bypassed synchronized role: %v", got)
	}
}

func TestOIDCBrowserFailureRedirectsConsumeFlowsAndAllowlistReasons(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	issuer := newTestIssuer(t)
	configuration := map[string]any{"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration", "client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true}
	saved := h.want(owner, "PUT", "/api/v3/oidc/configuration", configuration, nil, 200)
	for _, tc := range []struct {
		name, reason string
		claims       map[string]any
	}{
		{"cancelled", "cancelled", nil},
		{"denied", "denied", nil},
		{"provider", "provider", map[string]any{"aud": "wrong-client"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &browser{}
			authorization := h.want(b, "POST", "/api/v3/oidc/login", map[string]any{"return_to": "/settings/profile?tab=security"}, nil, 200)["authorization_url"].(string)
			callback := issuer.callback(t, authorization, tc.claims)
			if tc.name == "cancelled" {
				parsed, _ := url.Parse(callback)
				q := parsed.Query()
				q.Set("error", "access_denied")
				q.Set("error_description", "secret-provider-detail")
				q.Del("code")
				parsed.RawQuery = q.Encode()
				callback = parsed.String()
			}
			status, _, headers := h.request(b, "GET", callback, nil, map[string]string{"Accept": "text/html"})
			destination, _ := url.Parse(headers.Get("Location"))
			if status != 303 || destination.Path != "/login" || destination.Query().Get("oidc_error") != tc.reason || destination.Query().Get("return_to") != "/settings/profile?tab=security" || strings.Contains(destination.String(), "secret-provider-detail") {
				t.Fatalf("browser failure=%d %s", status, destination)
			}
			status, _, headers = h.request(b, "GET", callback, nil, map[string]string{"Accept": "text/html"})
			if status != 303 || headers.Get("Location") != "/login?oidc_error=expired" {
				t.Fatalf("consumed flow retry=%d %s", status, headers.Get("Location"))
			}
		})
	}
	// Browser conflict returns to the initiating profile. API callbacks still
	// receive problem JSON (covered by the concurrent linking test above).
	configuration["default_role"] = "viewer"
	h.want(owner, "PUT", "/api/v3/oidc/configuration", configuration, etagHeader(saved), 200)
	member := &browser{}
	authorization := h.want(member, "POST", "/api/v3/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
	h.want(member, "GET", issuer.callback(t, authorization, nil), nil, nil, 303)
	h.want(owner, "POST", "/api/v3/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_link"}, nil, 204)
	authorization = h.want(owner, "POST", "/api/v3/oidc/link", nil, nil, 200)["authorization_url"].(string)
	status, _, headers := h.request(owner, "GET", issuer.callback(t, authorization, nil), nil, map[string]string{"Accept": "text/html"})
	if status != 303 || headers.Get("Location") != "/settings/profile?oidc_error=link" {
		t.Fatalf("link failure=%d %s", status, headers.Get("Location"))
	}
}
