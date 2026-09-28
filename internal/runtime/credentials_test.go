package runtime

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
)

func credentialManager(t *testing.T, readAt time.Time, revoked ...string) *Manager {
	t.Helper()
	ineligible := map[string]Eligibility{}
	for _, id := range revoked {
		ineligible[id] = Revoked
	}
	return authorityManager(readAt, ineligible)
}

func authorityManager(readAt time.Time, ineligible map[string]Eligibility) *Manager {
	m := NewManager(nil, uuid.NewString(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.authority = authorityState{loaded: true, readAt: readAt, ineligible: ineligible}
	return m
}

func TestCredentialEligibilityNamesWhyAVersionCannotServe(t *testing.T) {
	live, revoked, lapsed := uuid.NewString(), uuid.NewString(), uuid.NewString()
	fresh := authorityManager(time.Now(), map[string]Eligibility{revoked: Revoked, lapsed: Lapsed})
	for id, want := range map[string]Eligibility{live: Eligible, revoked: Revoked, lapsed: Lapsed} {
		if got := fresh.Eligibility(id); got != want {
			t.Fatalf("credential eligibility %q, want %q", got, want)
		}
	}
	stale := authorityManager(time.Now().Add(-AuthorityStaleAfter-time.Second), map[string]Eligibility{revoked: Revoked, lapsed: Lapsed})
	for _, id := range []string{live, revoked, lapsed} {
		if got := stale.Eligibility(id); got != StaleAuthority {
			t.Fatalf("stale authority vouched for %s: %q", id, got)
		}
	}
	unloaded := NewManager(nil, uuid.NewString(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := unloaded.Eligibility(live); got != StaleAuthority {
		t.Fatalf("unloaded authority vouched for a credential: %q", got)
	}
}

func TestCredentialSourceServesOnlyEligibleSecrets(t *testing.T) {
	provider, slot, network, historical := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	release := &Release{credentials: map[string][]byte{slot: []byte("slot-secret"), network: []byte("network-secret")}}
	m := credentialManager(t, time.Now())
	if secret, _, err := m.Secret(t.Context(), release, slot); err != nil || string(secret) != "slot-secret" {
		t.Fatalf("installed slot credential: %q %v", secret, err)
	}
	if secret, err := m.NetworkSecret(t.Context(), release, provider, network); err != nil || string(secret) != "network-secret" {
		t.Fatalf("installed network credential: %q %v", secret, err)
	}
	// Without a secret authority (a mounted gateway), only what a release
	// installed can serve.
	for _, read := range []func() ([]byte, error){
		func() ([]byte, error) {
			secret, _, err := m.Secret(t.Context(), release, historical)
			return secret, err
		},
		func() ([]byte, error) { secret, _, err := m.Secret(t.Context(), nil, slot); return secret, err },
		func() ([]byte, error) { return m.NetworkSecret(t.Context(), release, provider, historical) },
	} {
		if secret, err := read(); !errors.Is(err, ErrCredentialUnavailable) || secret != nil {
			t.Fatalf("served a credential no release installed: %q %v", secret, err)
		}
	}
	revoked := credentialManager(t, time.Now(), slot, network)
	lapsed := authorityManager(time.Now(), map[string]Eligibility{slot: Lapsed, network: Revoked})
	stale := credentialManager(t, time.Now().Add(-AuthorityStaleAfter-time.Second))
	for _, m := range []*Manager{revoked, lapsed, stale} {
		if secret, _, err := m.Secret(t.Context(), release, slot); !errors.Is(err, ErrCredentialUnavailable) || secret != nil {
			t.Fatalf("served an ineligible slot credential: %q %v", secret, err)
		}
		if secret, err := m.NetworkSecret(t.Context(), release, provider, network); !errors.Is(err, ErrCredentialUnavailable) || secret != nil {
			t.Fatalf("served an ineligible network credential: %q %v", secret, err)
		}
	}
}

// A gateway asks workers to refresh the grant of a credential version the
// upstream refused, once per access token, but never a lapsed grant's: only a
// new grant enrollment replaces it.
func TestARefusedCredentialRequestsNoRefreshOfALapsedGrant(t *testing.T) {
	lapsed, revoked := uuid.NewString(), uuid.NewString()
	m := authorityManager(time.Now(), map[string]Eligibility{lapsed: Lapsed, revoked: Revoked})
	m.grants = map[string]servedGrant{lapsed: {generation: 3}, revoked: {generation: 1}}
	m.CredentialRefused(lapsed, 3)
	m.CredentialRefused(revoked, 1)
	if len(m.refreshRequested) != 0 {
		t.Fatalf("refreshes requested for versions that may not serve: %v", m.refreshRequested)
	}
}
