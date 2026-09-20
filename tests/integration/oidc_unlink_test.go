//go:build integration && oidctest

package integration_test

import "testing"

func TestOIDCUnlinkPreservesAnIdentityAuthorizedByCurrentMappings(t *testing.T) {
	for _, source := range []string{"email", "groups"} {
		t.Run(source, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			issuer := newTestIssuer(t)
			configuration := map[string]any{"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration", "client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true}
			mappingField, mappedValue, unmappedValue := "email_role_mappings", "mapped@example.com", "unmapped@example.com"
			if source == "groups" {
				mappingField, mappedValue, unmappedValue = "group_role_mappings", "mapped-group", "unmapped-group"
			}
			configuration[mappingField] = []any{map[string]string{"claim_value": mappedValue, "role": "developer"}}
			saved := h.want(owner, "PUT", "/api/v3/oidc/configuration", configuration, nil, 200)
			mapped := map[string]any{"sub": "mapped-subject", "email": "mapped@example.com", "groups": []string{"mapped-group"}}
			unmapped := map[string]any{"sub": "unmapped-subject", "email": "unmapped@example.com", "groups": []string{"unmapped-group"}}
			authorize := func(b *browser, path string, input any, claims map[string]any, status int) {
				t.Helper()
				authorization := h.want(b, "POST", path, input, nil, 200)["authorization_url"].(string)
				h.want(b, "GET", issuer.callback(t, authorization, claims), nil, nil, status)
			}
			member := &browser{}
			authorize(member, "/api/v3/oidc/login", map[string]any{}, mapped, 303)
			authorize(member, "/api/v3/oidc/reauthenticate", map[string]any{"purpose": "oidc_link"}, mapped, 303)
			authorize(member, "/api/v3/oidc/link", nil, unmapped, 303)

			identities := h.want(member, "GET", "/api/v3/oidc/identities", nil, nil, 200)["items"].([]any)
			if len(identities) != 2 {
				t.Fatalf("linked identities=%d, want 2", len(identities))
			}
			var mappedID string
			for _, value := range identities {
				item := value.(map[string]any)
				isMapped := item["email_at_link"] == "mapped@example.com"
				if isMapped {
					mappedID = item["id"].(string)
				}
				if item["can_unlink"] != !isMapped {
					t.Error("unlink availability counted an identity without a role mapping")
				}
			}
			authorize(member, "/api/v3/oidc/reauthenticate", map[string]any{"purpose": "oidc_unlink", "resource_id": mappedID}, mapped, 303)
			problem := h.want(member, "DELETE", "/api/v3/oidc/identities/"+mappedID, nil, nil, 409)
			if problem["type"] != "https://openllmproxy.dev/problems/last_sign_in_method" {
				t.Fatal("unlink was not rejected for removing the last usable identity")
			}
			var count, unlinks int
			if err := h.Pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM olp_go.oidc_identities),(SELECT count(*) FROM olp_go.audit WHERE action='oidc.unlink')").Scan(&count, &unlinks); err != nil || count != 2 || unlinks != 0 {
				t.Fatalf("identities=%d, unlink audits=%d, error=%v", count, unlinks, err)
			}
			h.want(member, "GET", "/api/v3/sessions/current", nil, nil, 200)
			authorize(&browser{}, "/api/v3/oidc/login", map[string]any{}, unmapped, 403)
			h.want(member, "GET", "/api/v3/sessions/current", nil, nil, 401)
			authorize(member, "/api/v3/oidc/login", map[string]any{}, mapped, 303)
			authorize(member, "/api/v3/oidc/reauthenticate", map[string]any{"purpose": "oidc_unlink", "resource_id": mappedID}, mapped, 303)
			h.want(member, "DELETE", "/api/v3/oidc/identities/"+mappedID, nil, nil, 409)

			// Eligibility follows the current mappings, not the role at link time.
			configuration[mappingField] = []any{map[string]string{"claim_value": unmappedValue, "role": "viewer"}}
			h.want(owner, "PUT", "/api/v3/oidc/configuration", configuration, etagHeader(saved), 200)
			identities = h.want(member, "GET", "/api/v3/oidc/identities", nil, nil, 200)["items"].([]any)
			for _, value := range identities {
				item := value.(map[string]any)
				if item["can_unlink"] != (item["id"] == mappedID) {
					t.Error("unlink availability did not follow the updated role mappings")
				}
			}
			// The rejected unlink must also roll back consumption of its recent-auth proof.
			h.want(member, "DELETE", "/api/v3/oidc/identities/"+mappedID, nil, nil, 204)
			login := &browser{}
			authorize(login, "/api/v3/oidc/login", map[string]any{}, unmapped, 303)
			if profile := h.want(login, "GET", "/api/v3/profile", nil, nil, 200); profile["role"] != "viewer" {
				t.Fatal("the remaining identity did not authorize sign-in with its current role")
			}
		})
	}
}
