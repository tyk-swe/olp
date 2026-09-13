//go:build integration

package integration_test

import "testing"

func TestOutstandingInvitationsLoseIssuerAuthority(t *testing.T) {
	for _, change := range []map[string]any{{"role": "developer"}, {"active": false}} {
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
			t.Fatal("outstanding grant was not retired")
		}
	}
}
