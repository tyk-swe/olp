//go:build integration

package integration_test

import (
	"testing"

	"github.com/tyk-swe/olp/internal/access"
)

func TestOutstandingInvitationsLoseIssuerAuthority(t *testing.T) {
	for _, change := range []map[string]any{{"role": "developer"}, {"active": false}, {"access_scope": "assigned"}} {
		h := newAccessHarness(t)
		owner := h.owner()
		issuer := h.invite(owner, "issuer@example.com", "owner")
		pending := h.want(issuer, "POST", "/api/v3/invitations", map[string]any{"email": "pending@example.com", "role": "owner"}, map[string]string{"Idempotency-Key": "pending"}, 201)
		profile := h.want(issuer, "GET", "/api/v3/profile", nil, nil, 200)
		h.want(owner, "PATCH", "/api/v3/users/"+profile["id"].(string), change, etagHeader(profile), 200)
		h.want(nil, "POST", "/api/v3/invitations/accept", map[string]any{"token": pending["token"], "display_name": "Pending", "password": accessPassword}, nil, 410)
		h.want(issuer, "GET", "/api/v3/sessions/current", nil, nil, 401)
		var revoked bool
		if err := h.Pool.QueryRow(t.Context(), "SELECT revoked_at IS NOT NULL FROM olp_go.invitations WHERE id=$1", pending["invitation"].(map[string]any)["id"]).Scan(&revoked); err != nil || !revoked {
			t.Fatal("outstanding grant was not retired", change)
		}
	}
}

func TestProvisionedOwnerDemotionRetiresIssuedInvitations(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	_, secret := createToken(h, owner, "provisioner", []string{"access"})
	path := "/api/v3/provisioning/scim-bridge/users/ext-owner"
	desired := map[string]any{"email": "issuer@example.com", "display_name": "Issuer", "role": "owner", "active": true}
	h.machineWant(secret, "PUT", path, desired, nil, 200)
	if _, err := access.RecoverPassword(t.Context(), h.Pool, "issuer@example.com", accessPassword); err != nil {
		t.Fatal(err)
	}
	issuer := login(h, "issuer@example.com")
	pending := h.want(issuer, "POST", "/api/v3/invitations", map[string]any{"email": "pending@example.com", "role": "owner"}, map[string]string{"Idempotency-Key": "pending"}, 201)
	desired["role"] = "viewer"
	h.machineWant(secret, "PUT", path, desired, nil, 200)
	h.want(nil, "POST", "/api/v3/invitations/accept", map[string]any{"token": pending["token"], "display_name": "Pending", "password": accessPassword}, nil, 410)
}

// Assigned owners cannot administer installation membership, which includes
// inspecting or revoking another member's sessions.
func TestAssignedOwnerCannotAdministerOtherSessions(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	scoped := h.invite(owner, "scoped@example.com", "owner")
	profile := h.want(scoped, "GET", "/api/v3/profile", nil, nil, 200)
	h.want(owner, "PATCH", "/api/v3/users/"+profile["id"].(string), map[string]any{"access_scope": "assigned"}, etagHeader(profile), 200)
	scoped = login(h, "scoped@example.com")
	ownerID := h.want(owner, "GET", "/api/v3/profile", nil, nil, 200)["id"].(string)
	h.want(scoped, "GET", "/api/v3/sessions?user_id="+ownerID, nil, nil, 403)
	session := h.want(owner, "GET", "/api/v3/sessions", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	h.want(scoped, "DELETE", "/api/v3/sessions/"+session["id"].(string), nil, nil, 403)
	h.want(owner, "GET", "/api/v3/sessions/current", nil, nil, 200)
	h.want(scoped, "GET", "/api/v3/sessions", nil, nil, 200)
	h.want(owner, "GET", "/api/v3/sessions?user_id="+profile["id"].(string), nil, nil, 200)
}
