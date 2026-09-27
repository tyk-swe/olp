package runtime

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/grants"
)

// servedGrant is what a gateway serves for the grant beneath a credential
// version: the version's secret, which holds the grant's current access
// token, as of the grant's generation.
type servedGrant struct {
	generation int64
	secret     []byte
}

// refreshGrants reloads the secrets of the installed release's credential
// versions whose grants workers refreshed since the last poll. A refresh
// advances its grant's generation, so a poll compares generations and reads
// only the secrets that changed, without a new release or an authority
// reload. It tells GrantRefreshed of each refreshed grant.
func (m *Manager) refreshGrants(ctx context.Context) error {
	if m.keys == nil {
		// Mounted gateways refuse credentials that have grants.
		return nil
	}
	providers := map[string]string{}
	for _, provider := range m.Release().Snapshot.Providers {
		for _, slot := range provider.Slots {
			if slot.CredentialID != nil {
				providers[*slot.CredentialID] = provider.ID
			}
		}
	}
	if len(providers) == 0 {
		m.serveGrants(nil)
		return nil
	}
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("grants: %w", err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "SELECT credential_id::text,generation FROM olp.provider_grants WHERE credential_id=ANY($1::uuid[])", slices.Collect(maps.Keys(providers)))
	if err != nil {
		return fmt.Errorf("grants: %w", err)
	}
	generations := map[string]int64{}
	for rows.Next() {
		var credentialID string
		var generation int64
		if err = rows.Scan(&credentialID, &generation); err != nil {
			rows.Close()
			return fmt.Errorf("grants: %w", err)
		}
		generations[credentialID] = generation
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return fmt.Errorf("grants: %w", err)
	}
	m.mu.RLock()
	known := m.grants
	m.mu.RUnlock()
	served := make(map[string]servedGrant, len(generations))
	var refreshed []string
	for credentialID, generation := range generations {
		previous, ok := known[credentialID]
		if ok && previous.generation == generation {
			served[credentialID] = previous
			continue
		}
		secret, err := m.keys.Read(ctx, tx, m.installation, credentialID, "provider_credential")
		if err != nil {
			return fmt.Errorf("grant of credential %s: %w", credentialID, err)
		}
		served[credentialID] = servedGrant{generation: generation, secret: secret}
		if ok {
			refreshed = append(refreshed, credentialID)
		}
	}
	m.serveGrants(served)
	for _, credentialID := range refreshed {
		m.log.Info("grant access token reloaded", "provider_id", providers[credentialID], "credential_id", credentialID, "generation", served[credentialID].generation)
		if m.GrantRefreshed != nil {
			m.GrantRefreshed(providers[credentialID], credentialID)
		}
	}
	return nil
}

// serveGrants replaces the grants the manager serves, forgetting the refresh
// requests of access tokens it no longer serves.
func (m *Manager) serveGrants(served map[string]servedGrant) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.grants = served
	maps.DeleteFunc(m.refreshRequested, func(credentialID string, generation int64) bool {
		return served[credentialID].generation != generation
	})
}

// grantSecret returns the secret the manager serves for a credential version
// with a grant: the one holding the grant's current access token.
func (m *Manager) grantSecret(credentialID string) ([]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	grant, ok := m.grants[credentialID]
	return grant.secret, ok
}

// CredentialRefused tells the manager that the upstream refused a credential
// version's secret. For a version with a grant, it asks workers to refresh the
// grant early, once per access token it served; the request runs apart from
// the caller.
func (m *Manager) CredentialRefused(credentialID string) {
	m.mu.Lock()
	grant, ok := m.grants[credentialID]
	requested := ok && m.refreshRequested[credentialID] == grant.generation
	if ok && !requested {
		m.refreshRequested[credentialID] = grant.generation
	}
	m.mu.Unlock()
	if !ok || requested {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), PollInterval)
		defer cancel()
		if err := grants.RequestRefresh(ctx, m.pool, credentialID, grant.generation); err != nil {
			m.log.Warn("grant refresh not requested", "credential_id", credentialID, "error", err)
			// A later refusal asks again.
			m.mu.Lock()
			if m.refreshRequested[credentialID] == grant.generation {
				delete(m.refreshRequested, credentialID)
			}
			m.mu.Unlock()
		}
	}()
}
