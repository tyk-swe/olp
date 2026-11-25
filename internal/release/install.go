package release

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	goruntime "runtime"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/guardrails/credential"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/routing"
)

func (m *Manager) refreshRelease(ctx context.Context) error {
	var id, digest, installation string
	var sequence int64
	var raw []byte
	err := m.runtimeReadPool().QueryRow(ctx, "SELECT r.id::text,r.sequence,r.sha256,r.snapshot,i.id::text FROM olp.runtime_releases r CROSS JOIN olp.installation i WHERE i.singleton ORDER BY r.sequence DESC LIMIT 1").Scan(&id, &sequence, &digest, &raw, &installation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	if installation != m.installation {
		return errors.New("runtime release database belongs to another installation")
	}
	for {
		current := m.desired.Load()
		if sequence <= current || m.desired.CompareAndSwap(current, sequence) {
			break
		}
	}
	m.mu.RLock()
	current := m.release.Sequence
	m.mu.RUnlock()
	if sequence <= current {
		return nil
	}
	candidate, err := m.install(ctx, id, sequence, digest, raw)
	if err != nil {
		m.mu.Lock()
		repeat := m.failed == sequence
		m.failed = max(m.failed, sequence)
		m.mu.Unlock()
		if repeat {
			return nil
		}
		return fmt.Errorf("release %d not installed: %w", sequence, err)
	}
	installed, err := m.publishRelease(ctx, candidate)
	if err != nil {
		return err
	}
	if installed {
		release := candidate.release
		m.log.Info("release installed", "sequence", sequence, "sha256", digest, "providers", release.Snapshot().ProviderCount(), "routes", release.Snapshot().RouteCount())
	}
	return nil
}

func (m *Manager) install(ctx context.Context, id string, sequence int64, digest string, raw []byte) (*releaseCandidate, error) {
	snapshot := &routing.Snapshot{}
	if err := json.Unmarshal(raw, snapshot); err != nil {
		return nil, err
	}
	if snapshot.Providers == nil {
		snapshot.Providers = map[string]routing.Provider{}
	}
	if snapshot.Routes == nil {
		snapshot.Routes = map[string]routing.Route{}
	}
	if err := snapshot.Compile(); err != nil {
		return nil, err
	}
	computed, err := snapshot.Digest()
	if err != nil {
		return nil, err
	}
	if !hmac.Equal([]byte(computed), []byte(digest)) {
		return nil, errors.New("snapshot digest mismatch")
	}
	release := &Release{ID: id, Sequence: sequence, Digest: digest, credentials: map[string][]byte{}}
	m.mu.RLock()
	candidate := &releaseCandidate{release: release, baseSequence: m.release.Sequence}
	knownGrants := m.grants
	m.mu.RUnlock()
	if m.keys == nil {
		credentials, err := installMounted(snapshot, m.mounted)
		if err != nil {
			return nil, err
		}
		release.credentials = credentials
		release.snapshot = snapshot.ReadOnly()
		return candidate, nil
	}
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := m.loadReleaseCredentials(ctx, tx, snapshot, release); err != nil {
		return nil, err
	}
	if err := m.loadReleaseGuardrails(ctx, tx, snapshot, release); err != nil {
		return nil, err
	}
	// A newly published grant credential must already have a coherent token
	// and generation. Requests can then gate and dispatch against the same
	// generation, and later polls can observe its advancement.
	release.snapshot = snapshot.ReadOnly()
	candidate.grants, err = m.readGrants(ctx, tx, grantProviders(release.Snapshot()), knownGrants)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	// Record store availability before publication so failed references produce
	// explicit route-plan decisions. Fetches hold neither transaction nor mutex.
	for id, reference := range release.references {
		value, _ := m.external.read(ctx, m.externalSecrets, id, reference)
		clear(value)
	}
	return candidate, nil
}

func (m *Manager) loadReleaseCredentials(ctx context.Context, tx pgx.Tx, snapshot *routing.Snapshot, release *Release) error {
	var credentialIDs []string
	for _, provider := range snapshot.Providers {
		for _, slot := range provider.Slots {
			if slot.CredentialID != nil {
				credentialIDs = append(credentialIDs, *slot.CredentialID)
			}
		}
	}
	var err error
	release.references, err = readExternalReferences(ctx, tx, credentialIDs)
	if err != nil {
		return err
	}
	for _, provider := range snapshot.Providers {
		if provider.Network != nil && provider.Network.CredentialID != "" {
			id := provider.Network.CredentialID
			var owner string
			if err := tx.QueryRow(ctx, "SELECT provider_id::text FROM olp.provider_network_credentials WHERE id=$1", id).Scan(&owner); err != nil || owner != provider.ID {
				return fmt.Errorf("provider %s network credential unavailable", provider.ID)
			}
			secret, err := m.keys.Read(ctx, tx, m.installation, id, secrets.ProviderCredential)
			if err != nil {
				return fmt.Errorf("provider %s network credential unavailable", provider.ID)
			}
			release.credentials[id] = secret
		}
		// Grant tokens rotate beneath the pinned credential version. Only
		// static credentials belong in the release's cache; Secret reads
		// grants from the poll or authority even after a newer publication.
		if provider.AuthMode == connectors.AuthGrant {
			continue
		}
		for _, slot := range provider.Slots {
			if slot.CredentialID == nil {
				continue
			}
			if _, external := release.references[*slot.CredentialID]; external {
				continue
			}
			if _, done := release.credentials[*slot.CredentialID]; done {
				continue
			}
			secret, err := m.keys.Read(ctx, tx, m.installation, *slot.CredentialID, secrets.ProviderCredential)
			if err != nil {
				return fmt.Errorf("credential %s for provider %s: %w", *slot.CredentialID, provider.ID, err)
			}
			release.credentials[*slot.CredentialID] = secret
		}
	}
	return nil
}

func (m *Manager) loadReleaseGuardrails(ctx context.Context, tx pgx.Tx, snapshot *routing.Snapshot, release *Release) error {
	if snapshot.Guardrails != nil {
		pins := &guardrailPluginPins{}
		goruntime.AddCleanup(release, func(p *guardrailPluginPins) { p.close() }, pins)
		pinned := map[string]bool{}
		for _, definition := range snapshot.Guardrails.Definitions {
			spec, err := guardrails.ParseRevision(definition)
			if err != nil {
				return errors.New("invalid guardrail configuration")
			}
			if plugin, ok := spec.(guardrails.PluginSpec); ok {
				if m.prepareGuardrailPlugin == nil {
					return errors.New("guardrail plugin unavailable")
				}
				if !pinned[plugin.Version] {
					unpin, err := m.prepareGuardrailPlugin(ctx, plugin.Version)
					if err != nil {
						return errors.New("guardrail plugin could not be preloaded")
					}
					pins.add(unpin)
					pinned[plugin.Version] = true
				}
			}
			for _, id := range spec.References().IDs() {
				secret, err := credential.Load(ctx, tx, m.keys, m.installation, definition.ProjectID, id)
				if err != nil {
					return err
				}
				release.credentials[id] = secret
			}
		}
	}
	return nil
}

// publishRelease advances release identity and its initial grant membership
// together. Grant polls keep making progress while installation performs remote
// I/O, but cannot publish old membership after this transition. A seed read
// before a concurrent rotation never replaces a newer cached token.
func (m *Manager) publishRelease(ctx context.Context, candidate *releaseCandidate) (bool, error) {
	m.grantRefresh.Lock()
	defer m.grantRefresh.Unlock()
	// grantRefresh serializes every release replacement, so current stays
	// installed while shared quota membership syncs outside m.mu.
	current := m.Release()
	// Waiting for a poll must not let cancelled or expired installation work
	// publish afterward. The last-good release remains owned by the manager.
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if candidate.release.Sequence <= current.Sequence {
		return false, nil
	}
	// An intervening release may have pruned a newer observed generation of
	// a credential this candidate brings back. Retry preparation against that
	// release instead of resurrecting an older token or retaining old secrets.
	if len(candidate.grants) > 0 && candidate.baseSequence != current.Sequence {
		return false, nil
	}
	if m.syncSharedQuotas != nil {
		retiredGroupIDs, err := m.sharedQuotaRetirements(ctx, current.Sequence, candidate.release.Sequence)
		if err != nil {
			return false, err
		}
		if err := m.syncSharedQuotas(ctx, current.Snapshot(), candidate.release.Snapshot(), retiredGroupIDs...); err != nil {
			return false, err
		}
	}
	m.mu.Lock()
	for credentialID, previous := range m.grants {
		if seeded, ok := candidate.grants[credentialID]; ok && previous.generation >= seeded.generation {
			candidate.grants[credentialID] = previous
		}
	}
	refreshed := advancedGrants(grantProviders(candidate.release.Snapshot()), m.grants, candidate.grants)
	m.serveGrantsLocked(candidate.grants)
	m.release = candidate.release
	m.mu.Unlock()
	m.grantsRefreshed(refreshed)
	return true, nil
}

func (m *Manager) sharedQuotaRetirements(ctx context.Context, current, next int64) ([]string, error) {
	if next-current <= 1 {
		return nil, nil
	}
	rows, err := m.runtimeReadPool().Query(ctx, `
		SELECT DISTINCT project_id, group_name
		FROM olp.runtime_shared_quota_retirements
		WHERE sequence > $1 AND sequence <= $2`, current, next)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var project, group string
		if err := row.Scan(&project, &group); err != nil {
			return "", err
		}
		return routing.SharedTokenQuotaGroupID(project, group), nil
	})
}
