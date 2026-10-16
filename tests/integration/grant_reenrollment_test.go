//go:build integration

package integration_test

import (
	"testing"

	"github.com/google/uuid"
)

// enrollSlot enrolls a grant for a provider's credential slot, re-enrolling
// the slot's grant if one backs it, signing in as whichever account the
// authority signs in next, and returns the completion.
func enrollSlot(t *testing.T, h *accessHarness, owner *browser, path, slot string) map[string]any {
	t.Helper()
	detail := h.want(owner, "GET", path, nil, nil, 200)
	enrollment := h.want(owner, "POST", path+"/grant-enrollments", map[string]any{"slot_id": slot}, etagHeader(detail), 201)
	if enrollment["slot_id"] != slot {
		t.Fatalf("enrollment %v backs another slot than %s", enrollment, slot)
	}
	return continueGrantEnrollment(h, owner, path, enrollment, signIn(t, enrollment).String(), 201)
}

// activateEnrolled validates the model access of newly enrolled slots and
// activates the provider draft, answering with the given status.
func activateEnrolled(t *testing.T, h *accessHarness, owner *browser, path string, status int, slots ...string) map[string]any {
	t.Helper()
	for _, slot := range slots {
		h.want(owner, "POST", path+"/credential-slots/"+slot+"/validate", nil, nil, 200)
	}
	detail := h.want(owner, "GET", path, nil, nil, 200)
	return h.want(owner, "POST", path+"/activate", nil, withMatch(detail, idem(uuid.NewString())), status)
}

// The same account: a new credential version, pending activation, that
// activates as a rotation.

// Another account changes the serving identity.

// A slot bound to the first account's version mixes principals, which
// activation refuses, naming each slot's, until that slot re-enrolls.

// A slot with no credential version yet, such as an imported one, enrolls
// its first grant the same way.
