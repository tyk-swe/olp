package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
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
	// Lapsed marks a credential version whose grant lapsed: it can no longer
	// be refreshed, and only a new grant enrollment replaces it.
	Lapsed Eligibility = "lapsed"
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
	// Secret returns a credential slot's usable secret and the generation
	// of the grant that supplied it, or zero for a static credential. The
	// generation is read together with the secret and travels with an attempt.
	Secret(ctx context.Context, release *Release, credentialID string) ([]byte, int64, error)
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
	return m.authority.ineligible[credentialID]
}

// ReadIneligible reads which credential versions and network credentials may
// not serve, and why, as key authority records it: an operator revoked them,
// which prevails, or a credential version's grant lapsed. It reads those among
// ids, or all of them when ids is nil.
func ReadIneligible(ctx context.Context, q access.Queryer, ids []string) (map[string]Eligibility, error) {
	rows, err := q.Query(ctx, `SELECT c.id::text,CASE WHEN c.revoked_at IS NOT NULL THEN 'revoked' ELSE 'lapsed' END
		FROM olp.provider_credentials c LEFT JOIN olp.provider_grants g ON g.credential_id=c.id
		WHERE (c.revoked_at IS NOT NULL OR g.lapsed_at IS NOT NULL) AND ($1::uuid[] IS NULL OR c.id=ANY($1))
		UNION ALL SELECT id::text,'revoked' FROM olp.provider_network_credentials
		WHERE revoked_at IS NOT NULL AND ($1::uuid[] IS NULL OR id=ANY($1))`, ids)
	if err != nil {
		return nil, err
	}
	ineligible := map[string]Eligibility{}
	for rows.Next() {
		var id string
		var eligibility Eligibility
		if err = rows.Scan(&id, &eligibility); err != nil {
			rows.Close()
			return nil, err
		}
		ineligible[id] = eligibility
	}
	rows.Close()
	return ineligible, rows.Err()
}

// Secret serves an eligible static credential from the release that installed
// it. Grant-backed versions use their current access token from the poll, or
// the secret authority when no longer polled. Static versions the release does
// not name, such as a historical revision's, also use the secret authority.
func (m *Manager) Secret(ctx context.Context, release *Release, credentialID string) ([]byte, int64, error) {
	if err := m.eligible(credentialID); err != nil {
		return nil, 0, err
	}
	if grant, ok := m.grantSecret(credentialID); ok {
		return grant.secret, grant.generation, nil
	}
	if secret, ok := release.Credential(credentialID); ok {
		return secret, 0, nil
	}
	if m.keys == nil {
		return nil, 0, fmt.Errorf("credential %s is not installed: %w", credentialID, ErrCredentialUnavailable)
	}
	// Historical credentials may not be among the grants this release
	// polls. Read their token and generation from the same database snapshot.
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(ctx)
	generations, err := ReadGrantGenerations(ctx, tx, []string{credentialID})
	if err != nil {
		return nil, 0, err
	}
	secret, err := m.keys.Read(ctx, tx, m.installation, credentialID, secrets.ProviderCredential)
	if err != nil {
		return nil, 0, fmt.Errorf("credential %s: %w: %w", credentialID, ErrCredentialUnavailable, err)
	}
	return secret, generations[credentialID], nil
}

// NetworkSecret serves an eligible network credential like Secret. A retained
// revision is not authority for its network identity, so one the release does
// not name is read as ReadNetworkSecret reads it.
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
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	return ReadNetworkSecret(ctx, tx, m.keys, m.installation, providerID, credentialID)
}

// ReadNetworkSecret reads the secret of a provider's network credential from
// the secret authority, where no release vouches for it: the credential must
// be the provider's and eligible, as key authority records it
// (ReadIneligible). A gateway reads so what its release does not name; a
// worker, which serves no release, reads so every network credential.
func ReadNetworkSecret(ctx context.Context, q access.Queryer, keys *secrets.KeyRing, installation, providerID, credentialID string) ([]byte, error) {
	unavailable := func(err error) error {
		return fmt.Errorf("network credential %s of provider %s: %w: %w", credentialID, providerID, ErrCredentialUnavailable, err)
	}
	var owner string
	if err := q.QueryRow(ctx, "SELECT provider_id::text FROM olp.provider_network_credentials WHERE id=$1", credentialID).Scan(&owner); err != nil {
		return nil, unavailable(err)
	}
	if owner != providerID {
		return nil, unavailable(errors.New("another provider's"))
	}
	ineligible, err := ReadIneligible(ctx, q, []string{credentialID})
	if err != nil {
		return nil, err
	}
	if eligibility := ineligible[credentialID]; eligibility != Eligible {
		return nil, unavailable(errors.New(string(eligibility)))
	}
	secret, err := keys.Read(ctx, q, installation, credentialID, secrets.ProviderCredential)
	if err != nil {
		return nil, unavailable(err)
	}
	return secret, nil
}

func (m *Manager) eligible(credentialID string) error {
	if eligibility := m.Eligibility(credentialID); eligibility != Eligible {
		return fmt.Errorf("credential %s: %s: %w", credentialID, eligibility, ErrCredentialUnavailable)
	}
	return nil
}
