//go:build integration && oidctest

package integration_test

import "testing"

func TestManagedAccountsKeepAnIdentityFromTheirIdP(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	issuer := newTestIssuer(t)
	h.want(owner, "PUT", "/api/v1/oidc/configuration", map[string]any{"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration", "client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true, "email_role_mappings": []any{map[string]string{"claim_value": "oidc@example.com", "role": "developer"}}}, nil, 200)
	f := newSAMLFixture(t)
	f.configure(t, h, owner)
	authorize := func(b *browser, path string, input any, claims map[string]any) {
		t.Helper()
		authorization := h.want(b, "POST", path, input, nil, 200)["authorization_url"].(string)
		h.want(b, "GET", issuer.callback(t, authorization, claims), nil, nil, 303)
	}
	saml := func(b *browser, path string, input map[string]any, subject, email string) {
		t.Helper()
		req, state := samlBegin(t, h, b, path, input)
		h.want(b, "GET", samlReceive(t, h, state, f.response(t, req, subject, email, []string{"developers"}, nil), 303), nil, nil, 303)
	}

	// An OIDC-managed account keeps its OIDC identity even with SAML to fall back on.
	claims := map[string]any{"sub": "oidc-subject", "email": "oidc@example.com"}
	member := &browser{}
	authorize(member, "/api/v1/oidc/login", map[string]any{}, claims)
	authorize(member, "/api/v1/oidc/reauthenticate", map[string]any{"purpose": "saml_link"}, claims)
	saml(member, "/api/v1/profile/saml/link", map[string]any{}, "oidc-member-saml", "oidc@example.com")
	identity := h.want(member, "GET", "/api/v1/oidc/identities", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	if identity["can_unlink"] != false {
		t.Fatal("the managing identity was offered for unlinking")
	}
	authorize(member, "/api/v1/oidc/reauthenticate", map[string]any{"purpose": "oidc_unlink", "resource_id": identity["id"]}, claims)
	h.want(member, "DELETE", "/api/v1/oidc/identities/"+identity["id"].(string), nil, nil, 409)

	// A SAML-managed account keeps its SAML identity even with OIDC to fall back on.
	worker := &browser{}
	saml(worker, "/api/v1/saml/login", map[string]any{}, "managed-worker", "saml@example.com")
	saml(worker, "/api/v1/profile/saml/reauthenticate", map[string]any{"purpose": "oidc_link"}, "managed-worker", "saml@example.com")
	authorize(worker, "/api/v1/oidc/link", nil, map[string]any{"sub": "saml-worker-oidc", "email": "saml@example.com"})
	samlID := h.want(worker, "GET", "/api/v1/profile/saml-identities", nil, nil, 200)["items"].([]any)[0].(map[string]any)["id"].(string)
	saml(worker, "/api/v1/profile/saml/reauthenticate", map[string]any{"purpose": "saml_unlink", "resource_id": samlID}, "managed-worker", "saml@example.com")
	h.want(worker, "DELETE", "/api/v1/profile/saml-identities/"+samlID, nil, nil, 409)
}
