//go:build integration && oidctest

package integration_test

import (
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"testing"

	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/gateway"
)

func TestEffectiveLocalPolicyPreservesTheOnlyOwner(t *testing.T) {
	for _, processDisabled := range []bool{false, true} {
		t.Run(fmt.Sprint("process-disabled=", processDisabled), func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			issuer := newTestIssuer(t)
			configuration := map[string]any{"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration", "client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true}
			saved := h.want(owner, "PUT", "/api/v3/oidc/configuration", configuration, nil, 200)
			h.want(owner, "POST", "/api/v3/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_link"}, nil, 204)
			authorization := h.want(owner, "POST", "/api/v3/oidc/link", nil, nil, 200)["authorization_url"].(string)
			claims := map[string]any{"sub": "owner-subject", "email": "owner@example.com"}
			h.want(owner, "GET", issuer.callback(t, authorization, claims), nil, nil, 303)
			// A locally provisioned account stays locally managed even without a mapping.
			var management string
			if err := h.Pool.QueryRow(t.Context(), "SELECT role_management FROM olp_go.users WHERE email='owner@example.com'").Scan(&management); err != nil || management != "local" {
				t.Fatalf("local ownership=%s: %v", management, err)
			}
			setting := h.want(owner, "GET", "/api/v3/settings/auth.local_login_enabled", nil, nil, 200)
			if !processDisabled {
				setting = h.want(owner, "PUT", "/api/v3/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(setting), 200)
			}
			h.Server.LocalLoginDisabled = processDisabled
			if h.want(nil, "GET", "/api/v3/auth/capabilities", nil, nil, 200)["local_login_enabled"] != false {
				t.Fatal("local login advertised against effective policy")
			}
			expected := 401
			if processDisabled {
				expected = 404
			}
			h.want(nil, "POST", "/api/v3/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, expected)
			identities := h.want(owner, "GET", "/api/v3/oidc/identities", nil, nil, 200)
			identity := identities["items"].([]any)[0].(map[string]any)
			if identity["can_unlink"] != false || identities["oidc_reauthentication_available"] != true {
				t.Fatal("identity capability ignored usable methods")
			}
			id := identity["id"].(string)
			h.want(owner, "POST", "/api/v3/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_unlink", "resource_id": id}, nil, 204)
			candidate := maps.Clone(configuration)
			candidate["enabled"] = false
			start := make(chan struct{})
			results := make(chan int, 2)
			first := &browser{Cookies: maps.Clone(owner.Cookies), CSRF: owner.CSRF}
			second := &browser{Cookies: maps.Clone(owner.Cookies), CSRF: owner.CSRF}
			go func() {
				<-start
				status, _, _ := h.request(first, "DELETE", "/api/v3/oidc/identities/"+id, nil, nil)
				results <- status
			}()
			go func() {
				<-start
				status, _, _ := h.request(second, "PUT", "/api/v3/oidc/configuration", candidate, etagHeader(saved))
				results <- status
			}()
			close(start)
			for range 2 {
				if status := <-results; status != 409 {
					t.Fatalf("concurrent method removal=%d", status)
				}
			}
			var identitiesCount, grants int
			if err := h.Pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM olp_go.oidc_identities),(SELECT count(*) FROM olp_go.recent_auth)").Scan(&identitiesCount, &grants); err != nil || identitiesCount != 1 || grants != 1 {
				t.Fatalf("rollback identities=%d grants=%d err=%v", identitiesCount, grants, err)
			}
			if current := h.want(owner, "GET", "/api/v3/oidc/configuration", nil, nil, 200); current["etag"] != saved["etag"] || current["enabled"] != true {
				t.Fatal("rejected disable was committed")
			}
			// With both switches enabled, the same unconsumed proof can remove OIDC.
			h.Server.LocalLoginDisabled = false
			if !processDisabled {
				h.want(owner, "PUT", "/api/v3/settings/auth.local_login_enabled", map[string]any{"value": "true"}, etagHeader(setting), 200)
			}
			h.want(owner, "DELETE", "/api/v3/oidc/identities/"+id, nil, nil, 204)
		})
	}
}

func TestInvitationAcceptanceRechecksEffectivePolicy(t *testing.T) {
	for _, processDisabled := range []bool{false, true} {
		t.Run(fmt.Sprint(processDisabled), func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			invitation := h.want(owner, "POST", "/api/v3/invitations", map[string]any{"email": "policy@example.com", "role": "viewer"}, map[string]string{"Idempotency-Key": "policy"}, 201)
			h.Server.LocalLoginDisabled = processDisabled
			if !processDisabled {
				if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.settings SET value='false' WHERE key='auth.local_login_enabled'"); err != nil {
					t.Fatal(err)
				}
			}
			h.want(owner, "POST", "/api/v3/invitations", map[string]any{"email": "second@example.com", "role": "viewer"}, map[string]string{"Idempotency-Key": "second"}, 409)
			input := map[string]any{"token": invitation["token"], "display_name": "Policy member", "password": accessPassword}
			h.want(nil, "POST", "/api/v3/invitations/accept", input, nil, 409)
			var consumed bool
			var users int
			if err := h.Pool.QueryRow(t.Context(), "SELECT accepted_at IS NOT NULL,(SELECT count(*) FROM olp_go.users WHERE email='policy@example.com') FROM olp_go.invitations WHERE id=$1", invitation["invitation"].(map[string]any)["id"]).Scan(&consumed, &users); err != nil || consumed || users != 0 {
				t.Fatalf("consumed=%v users=%d err=%v", consumed, users, err)
			}
			h.Server.LocalLoginDisabled = false
			if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.settings SET value='true' WHERE key='auth.local_login_enabled'"); err != nil {
				t.Fatal(err)
			}
			member := &browser{}
			h.want(member, "POST", "/api/v3/invitations/accept", input, nil, 201)
			h.want(member, "DELETE", "/api/v3/sessions/current", nil, nil, 204)
			h.want(member, "POST", "/api/v3/sessions", map[string]any{"email": "policy@example.com", "password": accessPassword}, nil, 201)
		})
	}
}

func TestManagementAdmissionUsesTrustedHopsAndBoundedAccountWindows(t *testing.T) {
	h := newAccessHarness(t)
	h.owner()
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8")}
	h.Server.ClientIP = func(r *http.Request) string { return gateway.ClientIP(r, trusted) }
	attempt := func(source, email string, status int) {
		t.Helper()
		h.want(nil, "POST", "/api/v3/sessions", map[string]any{"email": email, "password": "wrong"}, map[string]string{"X-Forwarded-For": source}, status)
	}
	for _, source := range []string{"203.0.113.1, 10.1.1.1", "2001:db8::2, 10.1.1.1"} {
		for range 5 {
			attempt(source, "independent@example.com", 401)
		}
		attempt(source, "independent@example.com", 429)
	}
	// Spoofed leftmost hops cannot escape the first untrusted hop.
	for i := 0; i < 6; i++ {
		status := 401
		if i == 5 {
			status = 429
		}
		attempt(fmt.Sprintf("198.51.100.%d, 203.0.113.3", i), "spoof@example.com", status)
	}
	for i := 0; i < 31; i++ {
		status := 401
		if i == 30 {
			status = 429
		}
		attempt(fmt.Sprintf("2001:db8:1::%x", i+1), "distributed@example.com", status)
	}
	// A denied attempt does not slide the window or permanently lock the account.
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.auth_admission SET window_started_at=now()-interval '61 seconds'"); err != nil {
		t.Fatal(err)
	}
	attempt("2001:db8:2::1", "distributed@example.com", 401)
	// The same forwarded headers are ignored through an untrusted direct peer.
	h.Server.ClientIP = func(r *http.Request) string {
		return gateway.ClientIP(r, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	}
	for i := 0; i < 6; i++ {
		status := 401
		if i == 5 {
			status = 429
		}
		attempt(fmt.Sprintf("203.0.113.%d", i), "untrusted@example.com", status)
	}
}

func TestAuthenticationOwnershipUpgradeAndSessionHints(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	member := h.invite(owner, "legacy@example.com", "viewer")
	// Recreate the previous schema and apply the real forward migration to legacy
	// rows: setup/invitation provenance survives; an ambiguous mixed row fails closed.
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp_go.users(id,email,display_name,password_hash,role,etag) VALUES('01980000-0000-7000-8000-000000000099','ambiguous@example.com','Ambiguous','legacy-hash','viewer','01980000-0000-7000-8000-000000000098');
 INSERT INTO olp_go.oidc_identities(id,user_id,issuer,subject) SELECT gen_random_uuid(),id,'https://issuer.test',email FROM olp_go.users;
 ALTER TABLE olp_go.users DROP COLUMN role_management, DROP COLUMN oidc_authorized;
 ALTER TABLE olp_go.sessions DROP COLUMN browser_hint;
 DELETE FROM olp_go.migrations WHERE version='0010_authentication_ownership.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(t.Context(), h.Pool); err != nil {
		t.Fatal(err)
	}
	for email, want := range map[string]string{"owner@example.com": "local", "legacy@example.com": "local", "ambiguous@example.com": "oidc"} {
		var got string
		if err := h.Pool.QueryRow(t.Context(), "SELECT role_management FROM olp_go.users WHERE email=$1", email).Scan(&got); err != nil || got != want {
			t.Fatalf("%s management=%s want=%s err=%v", email, got, want, err)
		}
	}
	sessions := h.want(member, "GET", "/api/v3/sessions", nil, nil, 200)["items"].([]any)
	if sessions[0].(map[string]any)["browser_hint"] != "Unknown browser" {
		t.Fatal("legacy metadata not represented")
	}
	b := &browser{}
	h.want(b, "POST", "/api/v3/sessions", map[string]any{"email": "legacy@example.com", "password": accessPassword}, map[string]string{"User-Agent": "Mozilla/5.0 (Windows NT 10.0) Chrome/140.0 Private untrusted <script>"}, 201)
	sessions = h.want(b, "GET", "/api/v3/sessions", nil, nil, 200)["items"].([]any)
	found := false
	for _, item := range sessions {
		row := item.(map[string]any)
		if row["current"] == true {
			found = true
			if row["browser_hint"] != "Chrome on Windows" {
				t.Fatalf("unsafe or missing hint: %v", row)
			}
		}
	}
	if !found {
		t.Fatal("current session missing")
	}
}
