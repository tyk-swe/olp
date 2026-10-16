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
