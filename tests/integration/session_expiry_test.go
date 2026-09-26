//go:build integration

package integration_test

import "testing"

func TestExpiredSessionCannotReadOrMutate(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.sessions SET expires_at=now()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	h.want(owner, "GET", "/api/v1/sessions/current", nil, nil, 401)
	h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "expired session"}, map[string]string{"Idempotency-Key": "expired-session"}, 401)
	h.want(owner, "POST", "/api/v1/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, 201)
	h.want(owner, "GET", "/api/v1/sessions/current", nil, nil, 200)
}
