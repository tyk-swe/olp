package runtime

import (
	"strconv"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/routing"
)

func grantCandidate(sequence, generation int64, credentialID string) *releaseCandidate {
	return &releaseCandidate{
		baseSequence: sequence - 1,
		release: &Release{Sequence: sequence, Snapshot: &routing.Snapshot{Providers: map[string]routing.Provider{
			"provider": {ID: "provider", AuthMode: connectors.AuthGrant, Slots: []routing.Slot{{CredentialID: &credentialID}}},
		}}},
		grants: map[string]servedGrant{credentialID: {generation: generation, secret: []byte(strconv.FormatInt(generation, 10))}},
	}
}

func publishGrantCandidate(t *testing.T, m *Manager, candidate *releaseCandidate) bool {
	t.Helper()
	installed, err := m.publishRelease(t.Context(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	return installed
}

// No database fallback is configured: publishing the pin alone cannot
// satisfy this read or expose its generation to pre-dispatch gating.

// The candidate's coherent read preceded a rotation poll. Preparing a
// release can be slow; its older seed must not undo that live progress.

// The next poll prepares the still-desired release against the new base
// and the primary's current generation instead of keeping retired secrets.

func TestReleasePublicationNotifiesOnlyAdvancedGrantGenerations(t *testing.T) {
	m := credentialManager(t, time.Now())
	notified := make(chan string, 3)
	m.GrantRefreshed = func(providerID, credentialID string) { notified <- providerID + "/" + credentialID }
	publishGrantCandidate(t, m, grantCandidate(1, 1, "credential"))
	publishGrantCandidate(t, m, grantCandidate(2, 2, "credential"))
	publishGrantCandidate(t, m, grantCandidate(3, 2, "credential"))
	m.Stop()
	if len(notified) != 1 || <-notified != "provider/credential" {
		t.Fatal("initial or unchanged seeds emitted a refresh, or advancement was not reported")
	}
}
