package runtime

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/routing"
)

func credentialManager(t *testing.T, readAt time.Time, revoked ...string) *Manager {
	t.Helper()
	ineligible := map[string]routing.Eligibility{}
	for _, id := range revoked {
		ineligible[id] = routing.Revoked
	}
	return authorityManager(readAt, ineligible)
}

func authorityManager(readAt time.Time, ineligible map[string]routing.Eligibility) *Manager {
	m := NewManager(nil, uuid.NewString(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.authority = authorityState{loaded: true, readAt: readAt, ineligible: ineligible}
	return m
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
	lapsed := authorityManager(time.Now(), map[string]routing.Eligibility{slot: routing.Lapsed, network: routing.Revoked})
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
