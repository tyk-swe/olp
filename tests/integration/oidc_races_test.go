//go:build integration && oidctest

package integration_test

import (
	"testing"
)

func TestOIDCManagedPasswordCannotEscapeVerifiedDeauthorization(t *testing.T) {
	h := newAccessHarness(t)
	bootstrap := h.owner()
	issuer := newTestIssuer(t)
	h.want(bootstrap, "PUT", "/api/v1/oidc/configuration", map[string]any{"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration", "client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true, "group_role_mappings": []any{map[string]string{"claim_value": "owners", "role": "owner"}, map[string]string{"claim_value": "readers", "role": "viewer"}}}, nil, 200)
	claims := map[string]any{"groups": []string{"owners"}}
	authorize := func(b *browser, path string, input any, claims map[string]any, status int) {
		t.Helper()
		authorization := h.want(b, "POST", path, input, nil, 200)["authorization_url"].(string)
		h.want(b, "GET", issuer.callback(t, authorization, claims), nil, nil, status)
	}
	member := &browser{}
	authorize(member, "/api/v1/oidc/login", map[string]any{}, claims, 303)
	profile := h.want(member, "GET", "/api/v1/profile", nil, nil, 200)
	authorize(member, "/api/v1/oidc/reauthenticate", map[string]any{"purpose": "password_enrollment"}, claims, 303)
	h.want(member, "POST", "/api/v1/profile/password/enroll", map[string]any{"new_password": accessPassword}, etagHeader(profile), 200)
	var management string
	if err := h.Pool.QueryRow(t.Context(), "SELECT role_management FROM olp.users WHERE id=$1", profile["id"]).Scan(&management); err != nil || management != "oidc" {
		t.Fatalf("ownership after enrollment=%s, err=%v", management, err)
	}
	identity := h.want(member, "GET", "/api/v1/oidc/identities", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	if identity["can_unlink"] != false {
		t.Fatal("password enrollment must not sever the external authorization source")
	}
	h.want(member, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_unlink", "resource_id": identity["id"]}, nil, 204)
	h.want(member, "DELETE", "/api/v1/oidc/identities/"+identity["id"].(string), nil, nil, 409)
	local := &browser{}
	h.want(local, "POST", "/api/v1/sessions", map[string]any{"email": "oidc@example.com", "password": accessPassword}, nil, 201)
	// Administrative guards still apply, but a verified external loss of the
	// final owner's groups must not preserve authority.
	bootstrapProfile := h.want(bootstrap, "GET", "/api/v1/profile", nil, nil, 200)
	h.want(member, "PATCH", "/api/v1/users/"+bootstrapProfile["id"].(string), map[string]any{"active": false}, etagHeader(bootstrapProfile), 200)
	invitation := h.want(member, "POST", "/api/v1/invitations", map[string]any{"email": "pending-denied@example.com", "role": "owner"}, map[string]string{"Idempotency-Key": "denied-invitation"}, 201)
	h.want(member, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_link"}, nil, 204)
	var sequence int64
	if err := h.Pool.QueryRow(t.Context(), "SELECT authority_sequence FROM olp.installation").Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	// An unverified token must not revoke anything.
	authorize(&browser{}, "/api/v1/oidc/login", map[string]any{}, map[string]any{"nonce": "incorrect", "groups": []string{}}, 403)
	h.want(member, "GET", "/api/v1/sessions/current", nil, nil, 200)
	// Distinct verified callbacks race to deny the same external identity.
	start := make(chan struct{})
	results := make(chan int, 2)
	for range 2 {
		b := &browser{}
		authorization := h.want(b, "POST", "/api/v1/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
		callback := issuer.callback(t, authorization, map[string]any{"groups": []string{}})
		go func() { <-start; status, _, _ := h.request(b, "GET", callback, nil, nil); results <- status }()
	}
	close(start)
	for range 2 {
		if status := <-results; status != 403 {
			t.Fatalf("denial=%d", status)
		}
	}
	h.want(member, "GET", "/api/v1/sessions/current", nil, nil, 401)
	h.want(local, "GET", "/api/v1/profile", nil, nil, 401)
	h.want(nil, "POST", "/api/v1/sessions", map[string]any{"email": "oidc@example.com", "password": accessPassword}, nil, 401)
	h.want(nil, "POST", "/api/v1/invitations/accept", map[string]any{"token": invitation["token"], "display_name": "Denied", "password": accessPassword}, nil, 410)
	var authorized bool
	var grants int
	var after int64
	if err := h.Pool.QueryRow(t.Context(), "SELECT oidc_authorized,(SELECT count(*) FROM olp.recent_auth),(SELECT authority_sequence FROM olp.installation) FROM olp.users WHERE id=$1", profile["id"]).Scan(&authorized, &grants, &after); err != nil || authorized || grants != 0 || after != sequence+1 {
		t.Fatalf("revocation: authorized=%v grants=%d sequence=%d->%d err=%v", authorized, grants, sequence, after, err)
	}
	// Recovery synchronizes the lower mapping, without resurrecting old sessions
	// or outstanding invitations, and permits the enrolled password again.
	recovered := &browser{}
	authorize(recovered, "/api/v1/oidc/login", map[string]any{}, map[string]any{"groups": []string{"readers"}}, 303)
	if got := h.want(recovered, "GET", "/api/v1/profile", nil, nil, 200)["role"]; got != "viewer" {
		t.Fatalf("recovered role=%v", got)
	}
	h.want(local, "GET", "/api/v1/sessions/current", nil, nil, 401)
	passwordSession := &browser{}
	h.want(passwordSession, "POST", "/api/v1/sessions", map[string]any{"email": "oidc@example.com", "password": accessPassword}, nil, 201)
	if got := h.want(passwordSession, "GET", "/api/v1/profile", nil, nil, 200)["role"]; got != "viewer" {
		t.Fatalf("password bypassed synchronized role: %v", got)
	}
}

// Browser conflict returns to the initiating profile. API callbacks still
// receive problem JSON (covered by the concurrent linking test above).
