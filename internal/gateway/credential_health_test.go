package gateway

import (
	"github.com/tyk-swe/olp/internal/runtime"
	"testing"
	"time"
)

func TestHalfOpenCredentialFailureReleasesSiblingProbe(t *testing.T) {
	now := time.Now()
	h := newHealthTracker(func() time.Time { return now })
	for range circuitFailures {
		h.record("provider", AttemptFact{Class: classConnect})
	}
	if !h.open("provider") {
		t.Fatal("endpoint circuit did not open")
	}
	now = now.Add(circuitOpenFor)
	_, first := h.claim("provider")
	_, second := h.claim("provider")
	if !first || second {
		t.Fatal("half-open probes were not exclusive")
	}
	h.record("provider", AttemptFact{Class: classCredential})
	if _, granted := h.claim("provider"); !granted {
		t.Fatal("credential rejection penalized sibling endpoint probe")
	}
	h.record("provider", AttemptFact{Class: classSuccess})
	if h.open("provider") {
		t.Fatal("successful probe did not close circuit")
	}
}
func TestCredentialRotationClears401ButRetains429(t *testing.T) {
	now := time.Now()
	h := newHealthTracker(func() time.Time { return now })
	oldID, newID := "old-version", "new-version"
	slot := runtime.Slot{ID: "logical-slot", CredentialID: &oldID}
	h.cooldown("provider", credentialHealthKey(&slot), credentialCooldown)
	slot.CredentialID = &newID
	if h.coolingDown("provider", credentialHealthKey(&slot)) {
		t.Fatal("old version sidelined rotated credential")
	}
	h.cooldown("provider", slot.ID, rateLimitCooldown)
	if !h.coolingDown("provider", slot.ID) {
		t.Fatal("rotation escaped logical-slot rate limit")
	}
}
