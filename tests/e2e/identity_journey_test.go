//go:build e2e

package e2e_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/tests/harness"
)

func TestMemberTokenInvitationAndProfileWritesReplayGuardStaleAndAudit(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	stale := map[string]string{"If-Match": `"00000000-0000-4000-8000-000000000000"`}
	refused := func(what string, caller *harness.Caller, method, path string, body any, headers map[string]string, status int, code string) {
		t.Helper()
		got, raw := s.Do(caller, method, path, body, headers)
		if got != status || !strings.Contains(string(raw), code) {
			t.Fatalf("%s: %s, want %d %s", what, describe(got, raw), status, code)
		}
	}
	recorded := func(what, resource, action string, times int) {
		t.Helper()
		actions := auditActions(t, s, resource)
		got := len(actions) - len(slices.DeleteFunc(actions, func(a string) bool { return a == action }))
		if got != times {
			t.Fatalf("%s: %s recorded %d times, want %d", what, action, got, times)
		}
	}
	identity := func(caller *harness.Caller) (user, session string) {
		t.Helper()
		current := s.Want(caller, "GET", "/api/v1/sessions/current", nil, nil, http.StatusOK)
		return current["user"].(map[string]any)["id"].(string), current["session_id"].(string)
	}
	ownerID, ownerSession := identity(owner)

	tokenInput := map[string]any{"name": "automation", "scopes": []string{"keys"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	tokenKey := harness.Idempotent()
	token := s.Want(owner, "POST", "/api/v1/management-tokens", tokenInput, tokenKey, http.StatusCreated)
	tokenID := token["id"].(string)
	if replayed := s.Want(owner, "POST", "/api/v1/management-tokens", tokenInput, tokenKey, http.StatusCreated); replayed["id"] != tokenID {
		t.Fatalf("token replay created %v, want %s", replayed["id"], tokenID)
	}
	refused("token with an unknown scope", owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "bad", "scopes": []string{"nothing"}}, harness.Idempotent(), http.StatusUnprocessableEntity, "scopes")
	revoke := "/api/v1/management-tokens/" + tokenID + "/revoke"
	refused("token revoke with a stale version", owner, "POST", revoke, nil, harness.Idempotent(stale), http.StatusPreconditionFailed, "")
	revokeKey := harness.Idempotent(harness.ETag(token))
	revoked := s.Want(owner, "POST", revoke, nil, revokeKey, http.StatusOK)
	if replayed := s.Want(owner, "POST", revoke, nil, revokeKey, http.StatusOK); replayed["etag"] != revoked["etag"] {
		t.Fatalf("token revoke replay returned %v, want %v", replayed["etag"], revoked["etag"])
	}
	refused("revoking a revoked token", owner, "POST", revoke, nil, harness.Idempotent(harness.ETag(revoked)), http.StatusConflict, "management_token_revoked")
	recorded("token", tokenID, "management_token.create", 1)
	recorded("token", tokenID, "management_token.revoke", 1)

	invitationInput := map[string]any{"email": "pending@example.com", "role": "viewer"}
	invitationKey := harness.Idempotent()
	invited := s.Want(owner, "POST", "/api/v1/invitations", invitationInput, invitationKey, http.StatusCreated)
	invitationID := invited["invitation"].(map[string]any)["id"].(string)
	if replayed := s.Want(owner, "POST", "/api/v1/invitations", invitationInput, invitationKey, http.StatusCreated); replayed["token"] != invited["token"] {
		t.Fatal("invitation replay issued a second token")
	}
	refused("inviting a member", owner, "POST", "/api/v1/invitations", map[string]any{"email": "owner@example.com", "role": "viewer"}, harness.Idempotent(), http.StatusConflict, "user_exists")
	s.Want(owner, "DELETE", "/api/v1/invitations/"+invitationID, nil, harness.Idempotent(), http.StatusOK)
	s.Want(owner, "DELETE", "/api/v1/invitations/"+invitationID, nil, harness.Idempotent(), http.StatusOK)
	recorded("invitation", invitationID, "invitation.create", 1)
	recorded("invitation", invitationID, "invitation.revoke", 1)

	member := s.Invite(owner, "member@example.com", "developer")
	memberID, _ := identity(member)
	memberPath := "/api/v1/users/" + memberID
	refused("a member revoking another member's session", member, "DELETE", "/api/v1/sessions/"+ownerSession, nil, nil, http.StatusNotFound, "")
	refused("member update with a stale version", owner, "PATCH", memberPath, map[string]any{"role": "operator"}, stale, http.StatusPreconditionFailed, "")
	promoted := s.Want(owner, "PATCH", memberPath, map[string]any{"role": "operator"}, harness.ETag(s.Want(owner, "GET", memberPath, nil, nil, http.StatusOK)), http.StatusOK)
	if promoted["role"] != "operator" {
		t.Fatalf("member role is %v, want operator", promoted["role"])
	}
	refused("a session that predates the role change", member, "GET", "/api/v1/profile", nil, nil, http.StatusUnauthorized, "")
	ownerPath := "/api/v1/users/" + ownerID
	refused("an owner demoting themselves", owner, "PATCH", ownerPath, map[string]any{"role": "viewer"}, harness.ETag(s.Want(owner, "GET", ownerPath, nil, nil, http.StatusOK)), http.StatusConflict, "cannot_change_current_user_access")
	recorded("member", memberID, "user.update", 1)

	directory := "/api/v1/provisioning/hr/users/e-1"
	provisioned := s.Want(owner, "PUT", directory, map[string]any{"email": "hired@example.com", "display_name": "Hired", "role": "viewer", "active": true}, nil, http.StatusOK)
	hiredID := provisioned["id"].(string)
	renamed := s.Want(owner, "PUT", directory, map[string]any{"email": "hired@example.com", "display_name": "Renamed", "role": "viewer", "active": true}, nil, http.StatusOK)
	if renamed["id"] != hiredID || renamed["display_name"] != "Renamed" {
		t.Fatalf("second provision returned %v", renamed)
	}
	refused("provisioning a taken email", owner, "PUT", "/api/v1/provisioning/hr/users/e-2", map[string]any{"email": "owner@example.com", "display_name": "Clash", "role": "viewer", "active": true}, nil, http.StatusConflict, "email_unavailable")
	if status, raw := s.Do(owner, "DELETE", directory, nil, nil); status != http.StatusNoContent {
		t.Fatalf("deprovision: %s", describe(status, raw))
	}
	if hired := s.Want(owner, "GET", "/api/v1/users/"+hiredID, nil, nil, http.StatusOK); hired["active"] != false {
		t.Fatalf("deprovisioned member is still active: %v", hired)
	}
	refused("deprovisioning an unknown identity", owner, "DELETE", "/api/v1/provisioning/hr/users/e-9", nil, nil, http.StatusNotFound, "")
	recorded("provisioned member", hiredID, "user.provision", 2)
	recorded("provisioned member", hiredID, "user.deprovision", 1)

	second := s.Login("owner@example.com")
	_, secondSession := identity(second)
	if status, raw := s.Do(owner, "DELETE", "/api/v1/sessions/"+secondSession, nil, nil); status != http.StatusNoContent {
		t.Fatalf("session revoke: %s", describe(status, raw))
	}
	refused("a revoked session", second, "GET", "/api/v1/profile", nil, nil, http.StatusUnauthorized, "")
	recorded("session", secondSession, "session.revoke", 1)

	refused("profile update with a stale version", owner, "PATCH", "/api/v1/profile", map[string]any{"display_name": "Renamed"}, stale, http.StatusPreconditionFailed, "")
	s.Want(owner, "PATCH", "/api/v1/profile", map[string]any{"display_name": "Renamed"}, harness.ETag(s.Want(owner, "GET", ownerPath, nil, nil, http.StatusOK)), http.StatusOK)
	if name := s.Want(owner, "GET", ownerPath, nil, nil, http.StatusOK)["display_name"]; name != "Renamed" {
		t.Fatalf("profile name is %v, want Renamed", name)
	}
	recorded("profile", ownerID, "profile.update", 1)

	reauthenticate := func(password, purpose string) map[string]any {
		return map[string]any{"current_password": password, "purpose": purpose}
	}
	refused("reauthentication with a wrong password", owner, "POST", "/api/v1/profile/reauthenticate", reauthenticate("not the password", "plugin_permit"), nil, http.StatusForbidden, "current_password_invalid")
	refused("reauthentication for an unknown purpose", owner, "POST", "/api/v1/profile/reauthenticate", reauthenticate(harness.Password, "anything"), nil, http.StatusUnprocessableEntity, "purpose")
	if status, raw := s.Do(owner, "POST", "/api/v1/profile/reauthenticate", reauthenticate(harness.Password, "plugin_permit"), nil); status/100 != 2 {
		t.Fatalf("reauthentication: %s", describe(status, raw))
	}
	recorded("reauthentication", ownerSession, "session.reauthenticate", 1)

	witness := s.Login("owner@example.com")
	change := map[string]any{"current_password": harness.Password, "new_password": "another long service test password"}
	refused("password change with a wrong password", owner, "POST", "/api/v1/profile/password", map[string]any{"current_password": "not the password", "new_password": change["new_password"]}, harness.ETag(s.Want(owner, "GET", ownerPath, nil, nil, http.StatusOK)), http.StatusForbidden, "current_password_invalid")
	refused("password change with a stale version", owner, "POST", "/api/v1/profile/password", change, stale, http.StatusPreconditionFailed, "")
	s.Want(owner, "POST", "/api/v1/profile/password", change, harness.ETag(s.Want(owner, "GET", ownerPath, nil, nil, http.StatusOK)), http.StatusOK)
	s.Want(owner, "GET", "/api/v1/profile", nil, nil, http.StatusOK)
	refused("another session after a password change", witness, "GET", "/api/v1/profile", nil, nil, http.StatusUnauthorized, "")
	recorded("password", ownerID, "profile.password.update", 1)
}
