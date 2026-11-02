package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secretstore"
)

type externalResolveFunc func(context.Context, secretstore.Reference) ([]byte, error)

func (f externalResolveFunc) Resolve(ctx context.Context, reference secretstore.Reference) ([]byte, error) {
	return f(ctx, reference)
}
func externalReference(version string) secretstore.Reference {
	return secretstore.Reference{Store: "gcp", SecretID: "projects/project/secrets/key", Version: version}
}

func TestExternalCredentialFailuresBecomePlanIneligibilityWithoutStaleFallback(t *testing.T) {
	reference := externalReference("1")
	m := &Manager{authority: authorityState{loaded: true, readAt: time.Now(), ineligible: map[string]routing.Eligibility{}}, release: &Release{references: map[string]secretstore.Reference{"credential": reference}}}
	var fail atomic.Bool
	m.ExternalSecrets = externalResolveFunc(func(context.Context, secretstore.Reference) ([]byte, error) {
		if fail.Load() {
			return nil, errors.New("private-store-failure")
		}
		return []byte("pinned-version-one"), nil
	})
	if err := m.refreshExternalCredentials(t.Context()); err != nil {
		t.Fatal(err)
	}
	secret, generation, err := m.Secret(t.Context(), m.release, "credential")
	if err != nil || string(secret) != "pinned-version-one" || generation != 0 || m.Eligibility("credential") != routing.Eligible {
		t.Fatal("pinned credential not served")
	}
	fail.Store(true)
	m.external.mu.Lock()
	m.external.entries["credential"].expires = time.Now().Add(-time.Second)
	m.external.mu.Unlock()
	if err = m.refreshExternalCredentials(t.Context()); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatal("resolution failure ignored")
	}
	if m.Eligibility("credential") != routing.ExternalUnavailable {
		t.Fatal("plan did not name external-store failure")
	}
	if secret, _, err = m.Secret(t.Context(), m.release, "credential"); !errors.Is(err, ErrCredentialUnavailable) || secret != nil {
		t.Fatal("expired bytes silently reused")
	}
	m.authority.ineligible["credential"] = routing.Revoked
	if m.Eligibility("credential") != routing.Revoked {
		t.Fatal("external failure hid revocation")
	}
	m.authority.readAt = time.Now().Add(-AuthorityStaleAfter - time.Second)
	if m.Eligibility("credential") != routing.StaleAuthority {
		t.Fatal("external cache bypassed stale authority")
	}
}

func TestExternalCredentialCacheExpiresOnItsClock(t *testing.T) {
	current := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	cache := &externalCredentials{now: func() time.Time { return current }}
	var calls atomic.Int64
	resolver := externalResolveFunc(func(context.Context, secretstore.Reference) ([]byte, error) {
		if calls.Add(1) == 1 {
			return []byte("first"), nil
		}
		return []byte("second"), nil
	})
	reference := externalReference("1")
	value, err := cache.read(t.Context(), resolver, "credential", reference)
	if err != nil || string(value) != "first" {
		t.Fatalf("first read = %q, %v", value, err)
	}
	current = current.Add(externalCredentialTTL - time.Second)
	value, err = cache.read(t.Context(), resolver, "credential", reference)
	if err != nil || string(value) != "first" || calls.Load() != 1 {
		t.Fatalf("read inside the TTL = %q, %v, calls %d, want the cached value", value, err, calls.Load())
	}
	current = current.Add(2 * time.Second)
	value, err = cache.read(t.Context(), resolver, "credential", reference)
	if err != nil || string(value) != "second" || calls.Load() != 2 {
		t.Fatalf("read after the TTL = %q, %v, calls %d, want a fresh resolution", value, err, calls.Load())
	}
}
