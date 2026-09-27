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
	m := NewManager(nil, uuid.NewString(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.authority = authorityState{loaded: true, readAt: readAt, revoked: map[string]struct{}{}}
	for _, id := range revoked {
		m.authority.revoked[id] = struct{}{}
	}
	return m
}

func TestCredentialEligibilityNamesWhyAVersionCannotServe(t *testing.T) {
	live, revoked := uuid.NewString(), uuid.NewString()
	fresh := credentialManager(t, time.Now(), revoked)
	if got := fresh.Eligibility(live); got != Eligible {
		t.Fatalf("unrevoked credential: %q", got)
	}
	if got := fresh.Eligibility(revoked); got != Revoked {
		t.Fatalf("revoked credential: %q", got)
	}
	stale := credentialManager(t, time.Now().Add(-AuthorityStaleAfter-time.Second), revoked)
	for _, id := range []string{live, revoked} {
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
	if secret, err := m.Secret(t.Context(), release, slot); err != nil || string(secret) != "slot-secret" {
		t.Fatalf("installed slot credential: %q %v", secret, err)
	}
	if secret, err := m.NetworkSecret(t.Context(), release, provider, network); err != nil || string(secret) != "network-secret" {
		t.Fatalf("installed network credential: %q %v", secret, err)
	}
	// Without a secret authority (a mounted gateway), only what a release
	// installed can serve.
	for _, read := range []func() ([]byte, error){
		func() ([]byte, error) { return m.Secret(t.Context(), release, historical) },
		func() ([]byte, error) { return m.Secret(t.Context(), nil, slot) },
		func() ([]byte, error) { return m.NetworkSecret(t.Context(), release, provider, historical) },
	} {
		if secret, err := read(); !errors.Is(err, ErrCredentialUnavailable) || secret != nil {
			t.Fatalf("served a credential no release installed: %q %v", secret, err)
		}
	}
	revoked := credentialManager(t, time.Now(), slot, network)
	stale := credentialManager(t, time.Now().Add(-AuthorityStaleAfter-time.Second))
	for _, m := range []*Manager{revoked, stale} {
		if secret, err := m.Secret(t.Context(), release, slot); !errors.Is(err, ErrCredentialUnavailable) || secret != nil {
			t.Fatalf("served an ineligible slot credential: %q %v", secret, err)
		}
		if secret, err := m.NetworkSecret(t.Context(), release, provider, network); !errors.Is(err, ErrCredentialUnavailable) || secret != nil {
			t.Fatalf("served an ineligible network credential: %q %v", secret, err)
		}
	}
}
