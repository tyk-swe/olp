package runtime

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
)

func grantCandidate(sequence, generation int64, credentialID string) *releaseCandidate {
	return &releaseCandidate{
		baseSequence: sequence - 1,
		release: &Release{Sequence: sequence, Snapshot: &Snapshot{Providers: map[string]Provider{
			"provider": {ID: "provider", AuthMode: connectors.AuthGrant, Slots: []Slot{{CredentialID: &credentialID}}},
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

func TestReleasePublicationSeedsGrantBeforeServing(t *testing.T) {
	m := credentialManager(t, time.Now())
	if !publishGrantCandidate(t, m, grantCandidate(1, 1, "credential")) {
		t.Fatal("initial release was not published")
	}
	// No database fallback is configured: publishing the pin alone cannot
	// satisfy this read or expose its generation to pre-dispatch gating.
	secret, generation, err := m.Secret(t.Context(), m.Release(), "credential")
	if err != nil || string(secret) != "1" || generation != 1 || m.GrantGeneration("credential") != generation {
		t.Fatalf("new release lacks a coherent initial grant: %q, generation %d, %v", secret, generation, err)
	}
}

func TestReleasePublicationPreservesConcurrentGrantAdvanceAndRejectsOlderRelease(t *testing.T) {
	m := credentialManager(t, time.Now())
	publishGrantCandidate(t, m, grantCandidate(1, 1, "credential"))
	// The candidate's coherent read preceded a rotation poll. Preparing a
	// release can be slow; its older seed must not undo that live progress.
	candidate := grantCandidate(2, 1, "credential")
	m.serveGrants(map[string]servedGrant{"credential": {generation: 2, secret: []byte("2")}})
	if !publishGrantCandidate(t, m, candidate) {
		t.Fatal("newer release was not published")
	}
	secret, generation, err := m.Secret(t.Context(), m.Release(), "credential")
	if err != nil || string(secret) != "2" || generation != 2 {
		t.Fatalf("stale seed replaced a rotated token: %q, generation %d, %v", secret, generation, err)
	}
	if publishGrantCandidate(t, m, grantCandidate(1, 3, "credential")) || m.Release().Sequence != 2 || m.GrantGeneration("credential") != 2 {
		t.Fatal("older installation replaced a newer release or its grant")
	}
}

func TestReleasePublicationPrunesOnlyRemovedGrantMembership(t *testing.T) {
	m := credentialManager(t, time.Now())
	publishGrantCandidate(t, m, grantCandidate(1, 1, "old"))
	m.refreshRequested["old"] = 1
	publishGrantCandidate(t, m, grantCandidate(2, 1, "replacement"))
	if m.GrantGeneration("old") != 0 || m.GrantGeneration("replacement") != 1 || len(m.refreshRequested) != 0 {
		t.Fatal("credential replacement retained old poll membership or lost its seed")
	}
}

func TestReleasePublicationRetriesSeedAfterMembershipChanged(t *testing.T) {
	m := credentialManager(t, time.Now())
	publishGrantCandidate(t, m, grantCandidate(1, 1, "credential"))
	later := grantCandidate(3, 1, "credential")
	later.baseSequence = 1
	m.serveGrants(map[string]servedGrant{"credential": {generation: 2, secret: []byte("2")}})
	publishGrantCandidate(t, m, grantCandidate(2, 1, "replacement"))
	if publishGrantCandidate(t, m, later) || m.Release().Sequence != 2 || m.GrantGeneration("credential") != 0 {
		t.Fatal("candidate resurrected a stale token after newer membership pruned it")
	}
	// The next poll prepares the still-desired release against the new base
	// and the primary's current generation instead of keeping retired secrets.
	if !publishGrantCandidate(t, m, grantCandidate(3, 2, "credential")) || m.GrantGeneration("credential") != 2 {
		t.Fatal("candidate did not converge after fresh preparation")
	}
}

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

func TestConcurrentServingNeverObservesPublishedGrantWithoutItsSeed(t *testing.T) {
	m := credentialManager(t, time.Now())
	done, failures := make(chan struct{}), make(chan error, 1)
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				release := m.Release()
				if release.Sequence == 0 {
					continue
				}
				secret, generation, err := m.Secret(t.Context(), release, "credential")
				if err != nil || generation < release.Sequence || string(secret) != strconv.FormatInt(generation, 10) {
					select {
					case failures <- fmt.Errorf("release %d observed token %q at generation %d: %w", release.Sequence, secret, generation, err):
					default:
					}
					return
				}
			}
		})
	}
	for sequence := int64(1); sequence <= 500; sequence++ {
		publishGrantCandidate(t, m, grantCandidate(sequence, sequence, "credential"))
	}
	close(done)
	readers.Wait()
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
}

func TestCancelledReleasePublicationPreservesPriorPinAndGrant(t *testing.T) {
	m := credentialManager(t, time.Now())
	publishGrantCandidate(t, m, grantCandidate(1, 1, "credential"))
	ctx, cancel := context.WithCancel(t.Context())
	blocked, finished := make(chan struct{}), make(chan error, 1)
	m.grantRefresh.Lock()
	go func() {
		close(blocked)
		installed, err := m.publishRelease(ctx, grantCandidate(2, 2, "credential"))
		if installed {
			finished <- errors.New("cancelled candidate was published")
			return
		}
		finished <- err
	}()
	<-blocked
	cancel()
	m.grantRefresh.Unlock()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal("installation ignored cancellation during publication wait", err)
	}
	if m.Release().Sequence != 1 || m.GrantGeneration("credential") != 1 {
		t.Fatal("cancelled installation changed the prior serving state")
	}
}
