//go:build integration

package integration_test

import (
	"testing"

	"github.com/google/uuid"
)

// subscribeToLapses subscribes a webhook destination to grant lapses, allowing
// the loopback webhook through the server's egress policy, and returns a
// function that counts the deliveries enqueued so far.
func subscribeToLapses(t *testing.T, h *accessHarness, owner *browser) func() int {
	t.Helper()
	h.Server.Egress = alertPolicy()
	hook := newWebhookFixture(t)
	destination := h.want(owner, "POST", "/api/v1/notifications/destinations", map[string]any{"name": "lapses", "url": hook.URL}, idem(uuid.NewString()), 201)["id"].(string)
	h.want(owner, "POST", "/api/v1/notifications/rules", map[string]any{"name": "Lapses", "event": "provider.grant.lapsed", "destination_id": destination}, idem(uuid.NewString()), 201)
	return func() int {
		t.Helper()
		var deliveries int
		if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.notification_deliveries").Scan(&deliveries); err != nil {
			t.Fatal(err)
		}
		return deliveries
	}
}

// moveToBuild moves a provider draft to another build of its plugin and
// returns the draft.
func moveToBuild(t *testing.T, h *accessHarness, owner *browser, path, digest string) map[string]any {
	t.Helper()
	detail := h.want(owner, "GET", path, nil, nil, 200)
	moved := detail["configuration"].(map[string]any)
	moved["profile_revision"] = digest
	return h.want(owner, "PATCH", path, map[string]any{"name": detail["name"], "configuration": moved}, etagHeader(detail), 200)
}

// retirements returns the audited retirements of grants.
func retirements(t *testing.T, h *accessHarness, owner *browser) []any {
	t.Helper()
	return h.want(owner, "GET", "/api/v1/audit?action=provider.grant.retire", nil, nil, 200)["items"].([]any)
}

// A configuration plan reuses the slot's grant only for the build that
// enrolled it.

// The active revision pins the enrolling build and selects the grant,
// which keeps refreshing on its behalf.

// Re-enrolled through the new build, the provider activates with it, and
// no configuration uses the first grant any more: when it comes due, a
// worker retires it rather than refresh it.
