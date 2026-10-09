package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	m := &Manager{authority: authorityState{loaded: true, readAt: time.Now(), ineligible: map[string]Eligibility{}}, release: &Release{references: map[string]secretstore.Reference{"credential": reference}}}
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
	if err != nil || string(secret) != "pinned-version-one" || generation != 0 || m.Eligibility("credential") != Eligible {
		t.Fatal("pinned credential not served")
	}
	fail.Store(true)
	m.external.mu.Lock()
	m.external.entries["credential"].expires = time.Now().Add(-time.Second)
	m.external.mu.Unlock()
	if err = m.refreshExternalCredentials(t.Context()); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatal("resolution failure ignored")
	}
	if m.Eligibility("credential") != ExternalUnavailable {
		t.Fatal("plan did not name external-store failure")
	}
	if secret, _, err = m.Secret(t.Context(), m.release, "credential"); !errors.Is(err, ErrCredentialUnavailable) || secret != nil {
		t.Fatal("expired bytes silently reused")
	}
	m.authority.ineligible["credential"] = Revoked
	if m.Eligibility("credential") != Revoked {
		t.Fatal("external failure hid revocation")
	}
	m.authority.readAt = time.Now().Add(-AuthorityStaleAfter - time.Second)
	if m.Eligibility("credential") != StaleAuthority {
		t.Fatal("external cache bypassed stale authority")
	}
}

func TestExternalCredentialCacheSharesFetchesWithoutBlockingEligibility(t *testing.T) {
	var cache externalCredentials
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	resolver := externalResolveFunc(func(ctx context.Context, _ secretstore.Reference) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return []byte("pinned-value"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() {
			value, err := cache.read(t.Context(), resolver, "credential", externalReference("1"))
			if err != nil || string(value) != "pinned-value" {
				t.Error("concurrent resolution failed")
			}
		})
	}
	<-started
	checked := make(chan struct{})
	go func() { cache.available("credential", time.Now()); close(checked) }()
	select {
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("network IO blocked route eligibility")
	}
	close(release)
	workers.Wait()
	if calls.Load() != 1 {
		t.Fatalf("duplicate store fetches: %d", calls.Load())
	}
	if _, err := cache.read(t.Context(), resolver, "credential", externalReference("2")); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatal("one credential ID changed its pinned version")
	}
}

func TestExternalCredentialCacheIsBoundedAndReturnsIndependentBytes(t *testing.T) {
	var cache externalCredentials
	resolver := externalResolveFunc(func(context.Context, secretstore.Reference) ([]byte, error) { return []byte("credential"), nil })
	for i := range maxExternalCredentials + 10 {
		value, err := cache.read(t.Context(), resolver, fmt.Sprint(i), externalReference("1"))
		if err != nil {
			t.Fatal(err)
		}
		value[0] = 'x'
	}
	if len(cache.entries) != maxExternalCredentials {
		t.Fatal("cache grew beyond its bound")
	}
	value, err := cache.read(t.Context(), resolver, fmt.Sprint(maxExternalCredentials+9), externalReference("1"))
	if err != nil || string(value) != "credential" {
		t.Fatal("caller mutated cached bytes")
	}
}
