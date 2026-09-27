package runtime

import (
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/secrets"
)

// A gateway polls the grants of the credential versions its release serves
// with grants only: a release whose providers take no grant reads nothing
// from the database, with no transaction, on any poll.
func TestAReleaseWithoutGrantProvidersPollsNoGrants(t *testing.T) {
	ring, err := secrets.ParseRing([]byte(`{"active_version":1,"keys":[{"version":1,"key":"` + strings.Repeat("ab", 32) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// With no database pool, any query would panic.
	m := NewManager(nil, uuid.NewString(), nil, ring, slog.New(slog.NewTextHandler(io.Discard, nil)))
	credential := uuid.NewString()
	m.release = &Release{Snapshot: &Snapshot{Providers: map[string]Provider{
		"static": {ID: "static", Kind: connectors.KindPlugin, AuthMode: connectors.AuthStaticCredential, Slots: []Slot{{ID: "slot", CredentialID: &credential}}},
	}}}
	if err = m.refreshGrants(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// Ending the cooldowns of refreshed grants runs apart from the poll that
// found them refreshed, which a slow shared store must not hold up: one
// notifier at a time tells GrantRefreshed of every refreshed grant, including
// those later polls find meanwhile.
func TestPollsTellOfRefreshedGrantsWithoutWaiting(t *testing.T) {
	m := NewManager(nil, uuid.NewString(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	telling, release := make(chan struct{}, 3), make(chan struct{})
	var mu sync.Mutex
	told := map[string]string{}
	var running, most int
	m.GrantRefreshed = func(providerID, credentialID string) {
		mu.Lock()
		running++
		most = max(most, running)
		mu.Unlock()
		telling <- struct{}{}
		<-release
		mu.Lock()
		running--
		told[credentialID] = providerID
		mu.Unlock()
	}
	poll := func(refreshed map[string]string) {
		t.Helper()
		polled := make(chan struct{})
		go func() {
			defer close(polled)
			m.grantsRefreshed(refreshed)
		}()
		select {
		case <-polled:
		case <-time.After(5 * time.Second):
			t.Fatal("a poll waited for GrantRefreshed")
		}
	}
	poll(map[string]string{"first": "provider-a"})
	<-telling
	poll(map[string]string{"second": "provider-b", "third": "provider-a"})
	close(release)
	m.wg.Wait()
	if len(told) != 3 || told["first"] != "provider-a" || told["second"] != "provider-b" || told["third"] != "provider-a" || most != 1 {
		t.Fatalf("told of %v, by up to %d notifiers at once", told, most)
	}
}
