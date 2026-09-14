package runtime

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

// Production guarantees: authority is polled every five seconds and a read
// older than sixty seconds (measured monotonically from the read's start)
// stops new admissions. Releases are polled on the same cadence but installed
// independently, so key and credential revocation never wait on a release
// that a gateway cannot install.
const (
	PollInterval        = 5 * time.Second
	AuthorityStaleAfter = 60 * time.Second
)

// Sentinel admission errors.
var (
	ErrStaleAuthority = errors.New("key authority is stale")
	ErrInvalidKey     = errors.New("invalid API key")
)

// Release is one installed snapshot pinned for the lifetime of a request.
type Release struct {
	ID          string
	Sequence    int64
	Digest      string
	Snapshot    *Snapshot
	InstalledAt time.Time
	credentials map[string][]byte
}

// Credential returns the plaintext credential referenced by a slot.
// NewRelease builds an installed release directly from a snapshot and its
// credentials for fixtures and tests that bypass the database.
func NewRelease(id string, sequence int64, snapshot *Snapshot, credentials map[string][]byte) (*Release, error) {
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	digest, err := snapshot.Digest()
	if err != nil {
		return nil, err
	}
	return &Release{ID: id, Sequence: sequence, Digest: digest, Snapshot: snapshot, InstalledAt: time.Now(), credentials: maps.Clone(credentials)}, nil
}

func (r *Release) Credential(id string) ([]byte, bool) {
	secret, ok := r.credentials[id]
	return secret, ok
}

// AuthorityStatus is exposed for health reporting.
type AuthorityStatus struct {
	Loaded   bool
	ID       string
	Sequence int64
	ReadAt   time.Time
	Stale    bool
}

type keyRecord struct {
	authority access.Authority
	digest    []byte
}

type authorityState struct {
	loaded   bool
	readAt   time.Time
	id       string
	sequence int64
	keys     map[string]keyRecord
	revoked  map[string]struct{}
}

// Manager installs releases and refreshes key authority for one gateway.
type Manager struct {
	pool         *pgxpool.Pool
	installation string
	auth         *secrets.AuthKey
	keys         *secrets.KeyRing
	log          *slog.Logger

	mu        sync.RWMutex
	authority authorityState
	release   *Release
	failed    int64

	stop chan struct{}
	wg   sync.WaitGroup
}

// NewManager prepares a manager that serves the empty snapshot until the
// first release installs and rejects admissions until authority loads.
func NewManager(pool *pgxpool.Pool, installation string, auth *secrets.AuthKey, keys *secrets.KeyRing, log *slog.Logger) *Manager {
	return &Manager{
		pool:         pool,
		installation: installation,
		auth:         auth,
		keys:         keys,
		log:          log,
		release:      emptyRelease(),
		stop:         make(chan struct{}),
	}
}

func emptyRelease() *Release {
	return &Release{Snapshot: &Snapshot{Providers: map[string]Provider{}, Routes: map[string]Route{}}, InstalledAt: time.Now()}
}

// Start performs one synchronous refresh and then polls until ctx ends or
// Stop is called. Refresh failures are logged; the gateway keeps serving its
// last installed state and fails closed once authority goes stale.
func (m *Manager) Start(ctx context.Context) {
	if err := m.Refresh(ctx); err != nil {
		m.log.Warn("runtime refresh failed", "error", err.Error())
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(PollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.stop:
				return
			case <-ticker.C:
				if err := m.Refresh(ctx); err != nil && ctx.Err() == nil {
					m.log.Warn("runtime refresh failed", "error", err.Error())
				}
			}
		}
	}()
}

// Stop ends polling and waits for the loop to exit.
func (m *Manager) Stop() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	m.wg.Wait()
}

// Refresh reloads authority and installs any newer release.
func (m *Manager) Refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, PollInterval)
	defer cancel()
	return errors.Join(m.refreshAuthority(ctx), m.refreshRelease(ctx))
}

func (m *Manager) refreshAuthority(ctx context.Context) error {
	start := time.Now()
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	defer tx.Rollback(ctx)
	var id string
	var sequence int64
	if err = tx.QueryRow(ctx, "SELECT COALESCE(authority_id::text,''),authority_sequence FROM olp_go.installation WHERE singleton").Scan(&id, &sequence); err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	m.mu.Lock()
	unchanged := m.authority.loaded && m.authority.id == id && m.authority.sequence == sequence
	if unchanged {
		m.authority.readAt = start
	}
	m.mu.Unlock()
	if unchanged {
		return nil
	}
	state := authorityState{loaded: true, readAt: start, id: id, sequence: sequence, keys: map[string]keyRecord{}, revoked: map[string]struct{}{}}
	rows, err := tx.Query(ctx, "SELECT id::text,lookup_id,created_by::text,digest,policy,expires_at,revoked_at FROM olp_go.api_keys")
	if err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	for rows.Next() {
		var policy []byte
		record := keyRecord{}
		if err = rows.Scan(&record.authority.ID, &record.authority.LookupID, &record.authority.Issuer, &record.digest, &policy, &record.authority.ExpiresAt, &record.authority.RevokedAt); err != nil {
			rows.Close()
			return fmt.Errorf("authority: %w", err)
		}
		if err = json.Unmarshal(policy, &record.authority.Policy); err != nil {
			rows.Close()
			return fmt.Errorf("authority: key %s policy: %w", record.authority.ID, err)
		}
		state.keys[record.authority.LookupID] = record
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	rows, err = tx.Query(ctx, "SELECT id::text FROM olp_go.provider_credentials WHERE revoked_at IS NOT NULL")
	if err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	for rows.Next() {
		var credential string
		if err = rows.Scan(&credential); err != nil {
			rows.Close()
			return fmt.Errorf("authority: %w", err)
		}
		state.revoked[credential] = struct{}{}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	m.mu.Lock()
	m.authority = state
	m.mu.Unlock()
	m.log.Info("authority refreshed", "sequence", sequence, "keys", len(state.keys), "revoked_credentials", len(state.revoked))
	return nil
}

func (m *Manager) refreshRelease(ctx context.Context) error {
	var id, digest string
	var sequence int64
	var raw []byte
	err := m.pool.QueryRow(ctx, "SELECT id::text,sequence,sha256,snapshot FROM olp_go.runtime_releases ORDER BY sequence DESC LIMIT 1").Scan(&id, &sequence, &digest, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	m.mu.RLock()
	current := m.release.Sequence
	m.mu.RUnlock()
	if sequence <= current {
		return nil
	}
	release, err := m.install(ctx, id, sequence, digest, raw)
	if err != nil {
		m.mu.Lock()
		repeat := m.failed == sequence
		m.failed = sequence
		m.mu.Unlock()
		if repeat {
			return nil
		}
		return fmt.Errorf("release %d not installed: %w", sequence, err)
	}
	m.mu.Lock()
	m.release = release
	m.mu.Unlock()
	m.log.Info("release installed", "sequence", sequence, "sha256", digest, "providers", len(release.Snapshot.Providers), "routes", len(release.Snapshot.Routes))
	return nil
}

func (m *Manager) install(ctx context.Context, id string, sequence int64, digest string, raw []byte) (*Release, error) {
	snapshot := &Snapshot{}
	if err := json.Unmarshal(raw, snapshot); err != nil {
		return nil, err
	}
	if snapshot.Providers == nil {
		snapshot.Providers = map[string]Provider{}
	}
	if snapshot.Routes == nil {
		snapshot.Routes = map[string]Route{}
	}
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	computed, err := snapshot.Digest()
	if err != nil {
		return nil, err
	}
	if !hmac.Equal([]byte(computed), []byte(digest)) {
		return nil, errors.New("snapshot digest mismatch")
	}
	release := &Release{ID: id, Sequence: sequence, Digest: digest, Snapshot: snapshot, InstalledAt: time.Now(), credentials: map[string][]byte{}}
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	for _, provider := range snapshot.Providers {
		for _, slot := range provider.Slots {
			if slot.CredentialID == nil {
				continue
			}
			if _, done := release.credentials[*slot.CredentialID]; done {
				continue
			}
			secret, err := m.keys.Read(ctx, tx, m.installation, *slot.CredentialID, "provider_credential")
			if err != nil {
				return nil, fmt.Errorf("credential %s for provider %s: %w", *slot.CredentialID, provider.ID, err)
			}
			release.credentials[*slot.CredentialID] = secret
		}
	}
	return release, nil
}

// Release returns the currently installed release for pinning.
func (m *Manager) Release() *Release {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.release
}

// Authenticate resolves an API key against the last authority read.
func (m *Manager) Authenticate(secret string) (access.Authority, error) {
	m.mu.RLock()
	state := m.authority
	m.mu.RUnlock()
	if !state.loaded || time.Since(state.readAt) > AuthorityStaleAfter {
		return access.Authority{}, ErrStaleAuthority
	}
	parts := strings.Split(secret, "_")
	if len(parts) != 3 || parts[0] != "olp" {
		return access.Authority{}, ErrInvalidKey
	}
	record, ok := state.keys[parts[1]]
	if !ok || !hmac.Equal(record.digest, m.auth.Digest("api_key", secret)) {
		return access.Authority{}, ErrInvalidKey
	}
	return record.authority, nil
}

// Revoked reports whether a credential version was revoked as of the last
// authority read. A stale authority reports every credential revoked.
func (m *Manager) Revoked(credentialID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.authority.loaded || time.Since(m.authority.readAt) > AuthorityStaleAfter {
		return true
	}
	_, revoked := m.authority.revoked[credentialID]
	return revoked
}

// Authority reports the last authority read for health output.
func (m *Manager) Authority() AuthorityStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := m.authority
	return AuthorityStatus{Loaded: a.loaded, ID: a.id, Sequence: a.sequence, ReadAt: a.readAt, Stale: !a.loaded || time.Since(a.readAt) > AuthorityStaleAfter}
}
