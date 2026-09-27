package runtime

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/connectors"
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
// reload. Only providers that authenticate with a grant have grants, so a
// release without any reads nothing. It tells GrantRefreshed of each
// refreshed grant, apart from the poll.
func (m *Manager) refreshGrants(ctx context.Context) error {
	if m.keys == nil {
		// Mounted gateways refuse credentials that have grants.
		return nil
	}
	providers := map[string]string{}
	for _, provider := range m.Release().Snapshot.Providers {
		if provider.AuthMode != connectors.AuthGrant {
			continue
		}
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
	refreshed := map[string]string{}
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
			refreshed[credentialID] = providers[credentialID]
		}
	}
	m.serveGrants(served)
	for credentialID, providerID := range refreshed {
		m.log.Info("grant access token reloaded", "provider_id", providerID, "credential_id", credentialID, "generation", served[credentialID].generation)
	}
	m.grantsRefreshed(refreshed)
	return nil
}

// grantsRefreshed tells GrantRefreshed of the grants a poll found refreshed,
// by credential version and provider, apart from the poll: ending a cooldown
// in the shared store may wait on it. One notifier at a time tells of them,
// and of those later polls find until it is done, so a slow store holds up
// no poll and gathers no goroutines.
func (m *Manager) grantsRefreshed(refreshed map[string]string) {
	if m.GrantRefreshed == nil || len(refreshed) == 0 {
		return
	}
	m.mu.Lock()
	idle := m.refreshedGrants == nil
	if idle {
		m.refreshedGrants = map[string]string{}
	}
	maps.Copy(m.refreshedGrants, refreshed)
	m.mu.Unlock()
	if idle {
		m.wg.Go(m.tellRefreshedGrants)
	}
}

// tellRefreshedGrants tells GrantRefreshed of refreshed grants until none is
// left to tell of.
func (m *Manager) tellRefreshedGrants() {
	for {
		m.mu.Lock()
		pending := m.refreshedGrants
		if len(pending) == 0 {
			m.refreshedGrants = nil
			m.mu.Unlock()
			return
		}
		m.refreshedGrants = map[string]string{}
		m.mu.Unlock()
		for credentialID, providerID := range pending {
			m.GrantRefreshed(providerID, credentialID)
		}
	}
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
// the caller. A version that may no longer serve, such as one whose grant
// lapsed, is not refreshed.
func (m *Manager) CredentialRefused(credentialID string) {
	if m.Eligibility(credentialID) != Eligible {
		return
	}
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
