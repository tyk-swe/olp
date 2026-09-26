//go:build integration && oidctest

package integration_test

import (
	"maps"
	"testing"
)

func TestOIDCConfigurationPreservesTheLastMappedOwner(t *testing.T) {
	for _, source := range []string{"default", "email", "groups"} {
		t.Run(source, func(t *testing.T) {
			h := newAccessHarness(t)
			bootstrap := h.owner()
			issuer := newTestIssuer(t)
			configuration := map[string]any{"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration", "client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true, "default_role": "viewer"}
			configuration["scopes"] = []string{"openid", "email", "profile", "groups"}
			mappingField := "default_role"
			switch source {
			case "default":
				configuration[mappingField] = "owner"
			case "email":
				mappingField = "email_role_mappings"
				configuration[mappingField] = []any{map[string]string{"claim_value": "oidc@example.com", "role": "owner"}}
			case "groups":
				mappingField = "group_role_mappings"
				configuration[mappingField] = []any{map[string]string{"claim_value": "developers", "role": "owner"}}
			}
			saved := h.want(bootstrap, "PUT", "/api/v1/oidc/configuration", configuration, nil, 200)
			login := func() *browser {
				b := &browser{}
				authorization := h.want(b, "POST", "/api/v1/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
				h.want(b, "GET", issuer.callback(t, authorization, nil), nil, nil, 303)
				if h.want(b, "GET", "/api/v1/profile", nil, nil, 200)["role"] != "owner" {
					t.Fatal("the remaining owner could not regain owner authority")
				}
				return b
			}
			owner := login()
			local := h.want(bootstrap, "GET", "/api/v1/profile", nil, nil, 200)
			h.want(owner, "PATCH", "/api/v1/users/"+local["id"].(string), map[string]any{"role": "viewer"}, etagHeader(local), 200)
			withoutSecret := maps.Clone(configuration)
			withoutSecret["client_secret"] = ""
			problem := h.want(owner, "PUT", "/api/v1/oidc/configuration", withoutSecret, etagHeader(saved), 409)
			if problem["type"] != "https://openllmproxy.dev/problems/last_usable_owner" {
				t.Fatal("removing the secret was not rejected by owner protection")
			}
			for _, role := range []string{"viewer", ""} {
				candidate := maps.Clone(configuration)
				candidate["client_secret"] = "must-not-survive-the-rejected-update"
				if source == "default" {
					candidate[mappingField] = nil
					if role != "" {
						candidate[mappingField] = role
					}
				} else {
					candidate[mappingField] = []any{}
					if role != "" {
						value := "oidc@example.com"
						if source == "groups" {
							value = "developers"
						}
						candidate[mappingField] = []any{map[string]string{"claim_value": value, "role": role}}
					}
				}
				h.want(owner, "PUT", "/api/v1/oidc/configuration", candidate, etagHeader(saved), 409)
			}
			for _, field := range []string{"client_id", "email_claim", "groups_claim"} {
				candidate := maps.Clone(configuration)
				candidate[field] = "unverified_claim"
				h.want(owner, "PUT", "/api/v1/oidc/configuration", candidate, etagHeader(saved), 409)
			}
			for _, scopes := range [][]string{{"openid"}, {"openid", "profile", "groups"}, {"openid", "email", "profile"}} {
				candidate := maps.Clone(configuration)
				candidate["scopes"] = scopes
				problem := h.want(owner, "PUT", "/api/v1/oidc/configuration", candidate, etagHeader(saved), 409)
				if problem["type"] != "https://openllmproxy.dev/problems/last_usable_owner" {
					t.Fatal("reducing scopes was not rejected by owner protection")
				}
			}
			current := h.want(owner, "GET", "/api/v1/oidc/configuration", nil, nil, 200)
			if current["etag"] != saved["etag"] || current["has_client_secret"] != true {
				t.Fatal("a rejected update changed the configuration or removed its secret")
			}
			var updates int
			if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp_go.audit WHERE action='oidc.configuration.update'").Scan(&updates); err != nil || updates != 1 {
				t.Fatal("a rejected mapping wrote a success audit")
			}
			if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.sessions SET expires_at=now()-interval '1 second'"); err != nil {
				t.Fatal(err)
			}
			owner = login()
			// A new unrelated mapping is safe; protecting the owner must not
			// freeze all role configuration for OIDC-only installations.
			groups, _ := configuration["group_role_mappings"].([]any)
			configuration["group_role_mappings"] = append(groups, map[string]string{"claim_value": "unrelated", "role": "viewer"})
			saved = h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, etagHeader(saved), 200)
			// Old identities must verify both the client and role inputs
			// before they can prove a usable owner sign-in path.
			for _, query := range []string{
				"UPDATE olp_go.oidc_identities SET role_claims=role_claims-'client_id'",
				"UPDATE olp_go.oidc_identities SET role_claims=role_claims-'scopes'",
				"UPDATE olp_go.oidc_identities SET role_claims=NULL",
			} {
				if _, err := h.Pool.Exec(t.Context(), query); err != nil {
					t.Fatal(err)
				}
				h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, etagHeader(saved), 409)
				owner = login()
				saved = h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, etagHeader(saved), 200)
			}
			identities := h.want(owner, "GET", "/api/v1/oidc/identities", nil, nil, 200)
			for _, item := range identities["items"].([]any) {
				if _, present := item.(map[string]any)["role_claims"]; present {
					t.Fatal("private role inputs escaped in identity metadata")
				}
			}
		})
	}
}

func TestOIDCSecretRemovalRequiresAnIndependentOwnerSignIn(t *testing.T) {
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

	// Omitting the secret keeps the existing credential available for sign-in.
	delete(configuration, "client_secret")
	saved = h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, etagHeader(saved), 200)
	if saved["has_client_secret"] != true {
		t.Fatal("omitting the secret removed the existing credential")
	}
	pending := &browser{}
	authorization = h.want(pending, "POST", "/api/v1/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
	configuration["client_secret"] = ""
	problem := h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, etagHeader(saved), 409)
	if problem["type"] != "https://openllmproxy.dev/problems/last_usable_owner" {
		t.Fatal("a password with local login disabled cannot authorize secret removal")
	}
	current := h.want(owner, "GET", "/api/v1/oidc/configuration", nil, nil, 200)
	if current["etag"] != saved["etag"] || current["has_client_secret"] != true {
		t.Fatal("rejected secret removal changed the configuration or credential")
	}
	var updates int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp_go.audit WHERE action='oidc.configuration.update'").Scan(&updates); err != nil || updates != 2 {
		t.Fatalf("configuration update audits=%d, error=%v", updates, err)
	}
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.sessions SET expires_at=now()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	h.want(pending, "GET", issuer.callback(t, authorization, claims), nil, nil, 303)
	owner = pending
	if profile := h.want(owner, "GET", "/api/v1/profile", nil, nil, 200); profile["role"] != "owner" {
		t.Fatal("the preserved flow and secret could not restore owner authority")
	}

	// An enabled local sign-in path permits removal, but must then stay enabled.
	localSetting = h.want(owner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "true"}, etagHeader(localSetting), 200)
	saved = h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, etagHeader(saved), 200)
	if saved["has_client_secret"] != false || h.want(owner, "GET", "/api/v1/oidc/configuration", nil, nil, 200)["has_client_secret"] != false {
		t.Fatal("secret removal did not delete the stored credential")
	}
	h.want(owner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(localSetting), 409)
	localOwner := &browser{}
	h.want(localOwner, "POST", "/api/v1/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, 201)
	configuration["client_secret"] = "write-only-client-secret"
	h.want(localOwner, "PUT", "/api/v1/oidc/configuration", configuration, etagHeader(saved), 200)
	// Restoring a credential must not revive evidence invalidated by removal.
	h.want(localOwner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(localSetting), 409)
	login := &browser{}
	authorization = h.want(login, "POST", "/api/v1/oidc/login", map[string]any{}, nil, 200)["authorization_url"].(string)
	h.want(login, "GET", issuer.callback(t, authorization, claims), nil, nil, 303)
	h.want(localOwner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(localSetting), 200)
	if profile := h.want(login, "GET", "/api/v1/profile", nil, nil, 200); profile["role"] != "owner" {
		t.Fatal("restoring the secret did not restore OIDC owner sign-in")
	}
}
