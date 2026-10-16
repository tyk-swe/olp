//go:build integration

package integration_test

import (
	"github.com/jackc/pgx/v5"
)

// Credential changes invalidate evidence even when the old model
// certifications or a historical revision are still available.

// Expanding access makes previously successful evidence stale.

// Discovery permission alone cannot validate a slot or a default rotation.

// auditOutcomes lists the recorded outcomes of action on one resource, oldest
// first.
func auditOutcomes(h *accessHarness, action, resourceID string) []string {
	h.t.Helper()
	rows, err := h.Pool.Query(h.t.Context(), "SELECT outcome FROM olp.audit WHERE action=$1 AND resource_id=$2 ORDER BY occurred_at,id", action, resourceID)
	if err != nil {
		h.t.Fatal(err)
	}
	outcomes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		h.t.Fatal(err)
	}
	return outcomes
}
