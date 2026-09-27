package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Eligibility is whether a credential version may serve and, when it may not,
// why. Planning, slot availability and dispatch all ask for it, so plan and
// attempt records name the same reason.
type Eligibility string

const (
	// Eligible is the only eligibility that serves.
	Eligible Eligibility = ""
	// Revoked marks a credential version an operator revoked.
	Revoked Eligibility = "revoked"
	// StaleAuthority withholds every credential version: the last authority
	// read is too old to vouch for any of them.
	StaleAuthority Eligibility = "stale_authority"
)

// ErrCredentialUnavailable reports a credential version the credential source
// cannot serve.
var ErrCredentialUnavailable = errors.New("credential unavailable")

// Credentials is the one credential source: serving paths ask it whether a
// credential version may serve and for the usable secret of one that may,
// never the release or the secret authority directly. The release is the
// caller's pinned release; a caller that pins none, such as media
// reconciliation, passes nil.
type Credentials interface {
	// Eligibility reports whether a credential version may serve now.
	Eligibility(credentialID string) Eligibility
	// Secret returns the usable secret of a credential slot's version.
	Secret(ctx context.Context, release *Release, credentialID string) ([]byte, error)
	// NetworkSecret returns the usable secret of a provider's network
	// credential.
	NetworkSecret(ctx context.Context, release *Release, providerID, credentialID string) ([]byte, error)
}

// Eligibility reports whether a credential version may serve as of the last
// authority read.
func (m *Manager) Eligibility(credentialID string) Eligibility {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.authority.loaded || time.Since(m.authority.readAt) > AuthorityStaleAfter {
		return StaleAuthority
	}
	if _, revoked := m.authority.revoked[credentialID]; revoked {
		return Revoked
	}
	return Eligible
}

// Secret serves an eligible credential version from the release that
// installed it, or with its grant's current access token when it has a grant.
// A version the release does not name, such as a historical revision's, is
// read from the secret authority.
func (m *Manager) Secret(ctx context.Context, release *Release, credentialID string) ([]byte, error) {
	if err := m.eligible(credentialID); err != nil {
		return nil, err
	}
	if secret, ok := m.grantSecret(credentialID); ok {
		return secret, nil
	}
	if secret, ok := release.Credential(credentialID); ok {
		return secret, nil
	}
	if m.keys == nil {
		return nil, fmt.Errorf("credential %s is not installed: %w", credentialID, ErrCredentialUnavailable)
	}
	secret, err := m.keys.Read(ctx, m.pool, m.installation, credentialID, "provider_credential")
	if err != nil {
		return nil, fmt.Errorf("credential %s: %w: %w", credentialID, ErrCredentialUnavailable, err)
	}
	return secret, nil
}

// NetworkSecret serves an eligible network credential like Secret. A retained
// revision is not authority for its network identity, so one the release does
// not name must still belong to the provider and be unrevoked.
func (m *Manager) NetworkSecret(ctx context.Context, release *Release, providerID, credentialID string) ([]byte, error) {
	if err := m.eligible(credentialID); err != nil {
		return nil, err
	}
	if secret, ok := release.Credential(credentialID); ok {
		return secret, nil
	}
	if m.keys == nil {
		return nil, fmt.Errorf("network credential %s is not installed: %w", credentialID, ErrCredentialUnavailable)
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var valid bool
	if err := tx.QueryRow(ctx, "SELECT revoked_at IS NULL FROM olp.provider_network_credentials WHERE id=$1 AND provider_id=$2", credentialID, providerID).Scan(&valid); err != nil || !valid {
		return nil, fmt.Errorf("network credential %s of provider %s: %w", credentialID, providerID, ErrCredentialUnavailable)
	}
	secret, err := m.keys.Read(ctx, tx, m.installation, credentialID, "provider_credential")
	if err != nil {
		return nil, fmt.Errorf("network credential %s: %w: %w", credentialID, ErrCredentialUnavailable, err)
	}
	return secret, nil
}

func (m *Manager) eligible(credentialID string) error {
	if eligibility := m.Eligibility(credentialID); eligibility != Eligible {
		return fmt.Errorf("credential %s: %s: %w", credentialID, eligibility, ErrCredentialUnavailable)
	}
	return nil
}
