//go:build integration

package integration_test

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

func TestOutstandingInvitationsLoseIssuerAuthority(t *testing.T) {
	for _, change := range []map[string]any{{"role": "developer"}, {"active": false}, {"access_scope": "assigned"}} {
		h := newAccessHarness(t)
		owner := h.owner()
		issuer := h.invite(owner, "issuer@example.com", "owner")
		pending := h.want(issuer, "POST", "/api/v1/invitations", map[string]any{"email": "pending@example.com", "role": "owner"}, map[string]string{"Idempotency-Key": "pending"}, 201)
		profile := h.want(issuer, "GET", "/api/v1/profile", nil, nil, 200)
		h.want(owner, "PATCH", "/api/v1/users/"+profile["id"].(string), change, etagHeader(profile), 200)
		h.want(nil, "POST", "/api/v1/invitations/accept", map[string]any{"token": pending["token"], "display_name": "Pending", "password": accessPassword}, nil, 410)
		h.want(issuer, "GET", "/api/v1/sessions/current", nil, nil, 401)
		var revoked bool
		if err := h.Pool.QueryRow(t.Context(), "SELECT revoked_at IS NOT NULL FROM olp.invitations WHERE id=$1", pending["invitation"].(map[string]any)["id"]).Scan(&revoked); err != nil || !revoked {
			t.Fatal("outstanding grant was not retired")
		}
	}
}

func TestProvisionedOwnerDemotionRetiresInvitations(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	_, secret := createToken(h, owner, "provisioner", []string{"access"})
	source := "/api/v1/provisioning/scim-bridge/users/ext-owner"
	provisioned := h.machineWant(secret, "PUT", source, map[string]any{
		"email": "ext-owner@example.com", "display_name": "External Owner", "role": "owner", "active": true,
	}, nil, 200)
	invitation := access.NewID()
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.invitations (id, email, role, digest, invited_by, expires_at)
	    VALUES ($1, 'pending@example.com', 'owner', $2, $3, $4)`,
		invitation, []byte("provisioned issuer digest"), provisioned["id"], time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	h.machineWant(secret, "PUT", source, map[string]any{
		"email": "ext-owner@example.com", "display_name": "External Owner", "role": "viewer", "active": true,
	}, nil, 200)
	var revoked bool
	if err := h.Pool.QueryRow(t.Context(), "SELECT revoked_at IS NOT NULL FROM olp.invitations WHERE id=$1", invitation).Scan(&revoked); err != nil || !revoked {
		t.Fatal("provisioned demotion did not retire outstanding grants", err)
	}
}
