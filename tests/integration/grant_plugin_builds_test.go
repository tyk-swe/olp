//go:build integration

package integration_test

import (
	"testing"
)

// moveToBuild moves a provider draft to another build of its plugin and
// returns the draft.
func moveToBuild(t *testing.T, h *accessHarness, owner *browser, path, digest string) map[string]any {
	t.Helper()
	detail := h.want(owner, "GET", path, nil, nil, 200)
	moved := detail["configuration"].(map[string]any)
	moved["profile_revision"] = digest
	return h.want(owner, "PATCH", path, map[string]any{"name": detail["name"], "configuration": moved}, etagHeader(detail), 200)
}

// A configuration plan reuses the slot's grant only for the build that
// enrolled it.

// The active revision pins the enrolling build and selects the grant,
// which keeps refreshing on its behalf.

// Re-enrolled through the new build, the provider activates with it, and
// no configuration uses the first grant any more: when it comes due, a
// worker retires it rather than refresh it.
