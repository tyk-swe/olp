//go:build integration && oidctest

package integration_test

import (
	"fmt"
	"maps"
	"testing"
)

func TestEffectiveLocalPolicyPreservesTheOnlyOwner(t *testing.T) {
	for _, processDisabled := range []bool{false, true} {
		t.Run(fmt.Sprint("process-disabled=", processDisabled), func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			issuer := newTestIssuer(t)
			configuration := map[string]any{"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration", "client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true}
			saved := h.want(owner, "PUT", "/api/v1/oidc/configuration", configuration, nil, 200)
			h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_link"}, nil, 204)
			authorization := h.want(owner, "POST", "/api/v1/oidc/link", nil, nil, 200)["authorization_url"].(string)
			claims := map[string]any{"sub": "owner-subject", "email": "owner@example.com"}
			h.want(owner, "GET", issuer.callback(t, authorization, claims), nil, nil, 303)
			// A locally provisioned account stays locally managed even without a mapping.
			var management string
			if err := h.Pool.QueryRow(t.Context(), "SELECT role_management FROM olp.users WHERE email='owner@example.com'").Scan(&management); err != nil || management != "local" {
				t.Fatalf("local ownership=%s: %v", management, err)
			}
			setting := h.want(owner, "GET", "/api/v1/settings/auth.local_login_enabled", nil, nil, 200)
			if !processDisabled {
				setting = h.want(owner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(setting), 200)
			}
			h.Server.LocalLoginDisabled = processDisabled
			if h.want(nil, "GET", "/api/v1/auth/capabilities", nil, nil, 200)["local_login_enabled"] != false {
				t.Fatal("local login advertised against effective policy")
			}
			expected := 401
			if processDisabled {
				expected = 404
			}
			h.want(nil, "POST", "/api/v1/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, expected)
			identities := h.want(owner, "GET", "/api/v1/oidc/identities", nil, nil, 200)
			identity := identities["items"].([]any)[0].(map[string]any)
			if identity["can_unlink"] != false || identities["oidc_reauthentication_available"] != true {
				t.Fatal("identity capability ignored usable methods")
			}
			id := identity["id"].(string)
			h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_unlink", "resource_id": id}, nil, 204)
			candidate := maps.Clone(configuration)
			candidate["enabled"] = false
			start := make(chan struct{})
			results := make(chan int, 2)
			first := &browser{Cookies: maps.Clone(owner.Cookies), CSRF: owner.CSRF}
			second := &browser{Cookies: maps.Clone(owner.Cookies), CSRF: owner.CSRF}
			go func() {
				<-start
				status, _, _ := h.request(first, "DELETE", "/api/v1/oidc/identities/"+id, nil, nil)
				results <- status
			}()
			go func() {
				<-start
				status, _, _ := h.request(second, "PUT", "/api/v1/oidc/configuration", candidate, etagHeader(saved))
				results <- status
			}()
			close(start)
			for range 2 {
				if status := <-results; status != 409 {
					t.Fatalf("concurrent method removal=%d", status)
				}
			}
			var identitiesCount, grants int
			if err := h.Pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM olp.oidc_identities),(SELECT count(*) FROM olp.recent_auth)").Scan(&identitiesCount, &grants); err != nil || identitiesCount != 1 || grants != 1 {
				t.Fatalf("rollback identities=%d grants=%d err=%v", identitiesCount, grants, err)
			}
			if current := h.want(owner, "GET", "/api/v1/oidc/configuration", nil, nil, 200); current["etag"] != saved["etag"] || current["enabled"] != true {
				t.Fatal("rejected disable was committed")
			}
			// With both switches enabled, the same unconsumed proof can remove OIDC.
			h.Server.LocalLoginDisabled = false
			if !processDisabled {
				h.want(owner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "true"}, etagHeader(setting), 200)
			}
			h.want(owner, "DELETE", "/api/v1/oidc/identities/"+id, nil, nil, 204)
		})
	}
}

// Spoofed leftmost hops cannot escape the first untrusted hop.

// A denied attempt does not slide the window or permanently lock the account.

// The same forwarded headers are ignored through an untrusted direct peer.
