package release

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/platform/secretstore"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/completeness"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
	"github.com/tyk-swe/olp/internal/workload"
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
	ErrPausedKey      = errors.New("api key is paused")
)

// Release is one installed snapshot pinned for the lifetime of a request.
type Release struct {
	ID          string
	Sequence    int64
	Digest      string
	snapshot    *routing.View
	credentials map[string][]byte
	references  map[string]secretstore.Reference
}

// releaseCandidate owns the state prepared before publication. Rotating grant
// seeds move into the manager's live credential cache, never the pinned release.
type releaseCandidate struct {
	release      *Release
	baseSequence int64
	grants       map[string]servedGrant
}

func NewRelease(id string, sequence int64, snapshot *routing.Snapshot, credentials map[string][]byte) (*Release, error) {
	if snapshot == nil {
		return nil, errors.New("snapshot is required")
	}
	frozen, err := snapshot.CompiledView()
	if err != nil {
		return nil, err
	}
	digest, err := frozen.Digest()
	if err != nil {
		return nil, err
	}
	owned := make(map[string][]byte, len(credentials))
	for id, secret := range credentials {
		owned[id] = append([]byte(nil), secret...)
	}
	return &Release{ID: id, Sequence: sequence, Digest: digest, snapshot: frozen, credentials: owned}, nil
}

func (r *Release) Snapshot() *routing.View {
	if r.snapshot == nil {
		return nil
	}
	view := *r.snapshot
	return &view
}

// Credential returns an independent plaintext credential for fixture setup.
// Serving paths ask the credential source, which borrows immutable bytes.
func (r *Release) Credential(id string) ([]byte, bool) {
	secret, ok := r.credential(id)
	return append([]byte(nil), secret...), ok
}

func (r *Release) credential(id string) ([]byte, bool) {
	if r == nil {
		return nil, false
	}
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
	expiresAt *time.Time
	authority keyauthority.Authority
	digest    []byte
}

type authorityState struct {
	mcpTools                  map[string]mcpToolAuthority
	agentPrincipals           map[string]keyauthority.Authority
	agentResources            map[string]agentResourceAuthority
	workloadIssuers           map[string]workloadIssuer
	workloadPrincipals        map[string]keyauthority.Authority
	budgetIncreases           map[string]string
	installationBudget        *keyauthority.BudgetPolicy
	projectOrganizations      map[string]string
	organizationBudgets       map[string]*keyauthority.BudgetPolicy
	projectBudgets            map[string]*keyauthority.BudgetPolicy
	projectAttributionBudgets map[string]keyauthority.AttributionBudgets
	loaded                    bool
	readAt                    time.Time
	id                        string
	sequence                  int64
	keys                      map[string]*keyRecord
	// ineligible holds the credential versions and network credentials that
	// may not serve, with why.
	ineligible map[string]routing.Eligibility
}

type ManagerConfig struct {
	Pool                   *pgxpool.Pool
	ReadPool               *pgxpool.Pool
	Installation           string
	Auth                   *secrets.AuthKey
	Keys                   *secrets.KeyRing
	Log                    *slog.Logger
	Hostname               string
	Region                 string
	ExternalSecrets        ExternalSecrets
	Mounted                map[string]MountedProvider
	PrepareGuardrailPlugin func(context.Context, string) (func(), error)
}

type Manager struct {
	prepareGuardrailPlugin func(context.Context, string) (func(), error)
	workloadKeys           *workload.Cache
	workloadRegistration   sync.Mutex
	pool                   *pgxpool.Pool
	readPool               *pgxpool.Pool
	region                 string
	externalSecrets        ExternalSecrets
	external               externalCredentials
	authorityRefresh       sync.Mutex
	grantRefresh           sync.Mutex
	replica                replicaProgress
	installation           string
	auth                   *secrets.AuthKey
	keys                   *secrets.KeyRing
	mounted                map[string]MountedProvider
	log                    *slog.Logger
	instanceID             string

	grantRefreshed func(providerID, credentialID string)

	mu        sync.RWMutex
	taskStore taskCapabilityStore
	authority authorityState
	release   *Release
	failed    int64
	inputs    *routinginputs.Inputs
	desired   atomic.Int64
	// grants holds what the manager serves for the installed release's
	// credential versions with grants, and refreshRequested the generation
	// whose access token each asked workers to refresh early.
	grants           map[string]servedGrant
	refreshRequested map[string]int64
	// refreshedGrants holds the refreshed grants, by credential version and
	// provider, that the notifier telling GrantRefreshed has yet to tell of;
	// it is non-nil while the notifier runs.
	refreshedGrants map[string]string

	stop       chan struct{}
	wg         sync.WaitGroup
	tasksMu    sync.Mutex
	taskCtx    context.Context
	cancelTask context.CancelFunc
	started    bool
	stopping   bool
	waitStop   sync.Once
	stopped    chan struct{}
}

func NewManager(config ManagerConfig) *Manager {
	tasks, cancel := context.WithCancel(context.Background())
	return &Manager{
		workloadKeys: workload.NewCache(access.IdentityHTTPClient()),
		pool:         config.Pool, readPool: config.ReadPool, installation: config.Installation,
		auth: config.Auth, keys: config.Keys, log: config.Log, region: config.Region,
		externalSecrets: config.ExternalSecrets, mounted: cloneMounted(config.Mounted),
		prepareGuardrailPlugin: config.PrepareGuardrailPlugin,
		release:                emptyRelease(), stop: make(chan struct{}), taskCtx: tasks,
		cancelTask: cancel, stopped: make(chan struct{}),
		instanceID:       completeness.GatewayInstance(config.Hostname),
		refreshRequested: map[string]int64{},
	}
}

func (m *Manager) Region() string { return m.region }

func emptyRelease() *Release {
	return &Release{snapshot: (&routing.Snapshot{Providers: map[string]routing.Provider{}, Routes: map[string]routing.Route{}}).ReadOnly()}
}

// Start performs one synchronous refresh and then polls until ctx ends or
// StopContext is called. Refresh failures are logged; the gateway keeps serving its
// last installed state and fails closed once authority goes stale.
func (m *Manager) Start(ctx context.Context, grantRefreshed func(providerID, credentialID string)) {
	m.tasksMu.Lock()
	if m.stopping || m.started {
		m.tasksMu.Unlock()
		return
	}
	m.started = true
	m.grantRefreshed = grantRefreshed
	m.wg.Add(1)
	m.tasksMu.Unlock()
	defer m.wg.Done()
	// Stop cancels in-flight refresh I/O as well as the polling select.
	stopParent := context.AfterFunc(ctx, m.cancelTask)
	if err := m.Refresh(m.taskCtx); err != nil {
		m.log.Warn("runtime refresh failed", "error", err.Error())
	}
	if !m.launch(func(ctx context.Context) {
		defer stopParent()
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
	}) {
		stopParent()
	}
}

// StopContext cancels all manager-owned tasks, refuses new tasks, and joins
// them within the caller's drain budget. A timed-out callback remains explicit
// uncertainty; this method never extends the process shutdown deadline.
func (m *Manager) StopContext(ctx context.Context) error {
	m.tasksMu.Lock()
	if !m.stopping {
		m.stopping = true
		close(m.stop)
		m.cancelTask()
	}
	m.tasksMu.Unlock()
	m.waitStop.Do(func() { go func() { m.wg.Wait(); close(m.stopped) }() })
	select {
	case <-m.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// launch registers ownership before a task starts, including grant refresh
// requests initiated by serving handlers during drain.
func (m *Manager) launch(task func(context.Context)) bool {
	m.tasksMu.Lock()
	defer m.tasksMu.Unlock()
	if m.stopping {
		return false
	}
	m.wg.Go(func() { task(m.taskCtx) })
	return true
}

// Refresh gives each independent freshness obligation its own polling budget.
// A slow release installation or remote secret cannot consume authority,
// grant, or measurement refresh time. Each state publisher rejects regressions.
func (m *Manager) Refresh(ctx context.Context) error {
	err := refreshObligations(ctx, PollInterval, m.refreshAuthority, m.refreshRelease, m.refreshGrants, m.refreshInputs, m.refreshExternalCredentials)
	status, cancel := context.WithTimeout(ctx, PollInterval)
	defer cancel()
	return errors.Join(err, m.recordInstallStatus(status))
}

func refreshObligations(ctx context.Context, budget time.Duration, obligations ...func(context.Context) error) error {
	errs := make([]error, len(obligations))
	var wg sync.WaitGroup
	for i, obligation := range obligations {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, budget)
			defer cancel()
			errs[i] = obligation(ctx)
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (m *Manager) Release() *Release {
	m.mu.RLock()
	defer m.mu.RUnlock()
	release := *m.release
	return &release
}

// EndUserDigest derives a project-scoped identity without retaining the identifier.
func (m *Manager) EndUserDigest(projectID *string, identifier string) string {
	return keyauthority.DigestEndUser(m.auth, projectID, identifier)
}

func (m *Manager) Digest(purpose secrets.DigestPurpose, value string) []byte {
	return m.auth.Digest(purpose, value)
}

// Authenticate resolves an API key against the last authority read. The
// authority it returns is shared with the installed key records: callers read
// it and copy it before changing it.
func (m *Manager) Authenticate(secret string) (*keyauthority.Authority, error) {
	// An API key needs only the key records, and copying the whole authority
	// would cost every request more than the lookup.
	m.mu.RLock()
	loaded, readAt, keys := m.authority.loaded, m.authority.readAt, m.authority.keys
	m.mu.RUnlock()
	if !loaded || time.Since(readAt) > AuthorityStaleAfter {
		return nil, ErrStaleAuthority
	}
	if !strings.HasPrefix(secret, "olp_") {
		return m.authenticateWorkload(secret)
	}
	presented := secret[len("olp_"):]
	separator := strings.IndexByte(presented, '_')
	if separator < 0 {
		return nil, ErrInvalidKey
	}
	record, ok := keys[presented[:separator]]
	if !ok {
		return nil, ErrInvalidKey
	}
	// Only a known lookup needs its secret suffix inspected and authenticated.
	if strings.IndexByte(presented[separator+1:], '_') >= 0 || record.authority.WorkloadIssuerID != nil || record.expiresAt != nil && !time.Now().Before(*record.expiresAt) || !hmac.Equal(record.digest, m.auth.Digest(secrets.APIKeyDigest, secret)) {
		return nil, ErrInvalidKey
	}
	return &record.authority, nil
}

// Authority reports the last authority read for health output.
func (m *Manager) Authority() AuthorityStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := m.authority
	return AuthorityStatus{Loaded: a.loaded, ID: a.id, Sequence: a.sequence, ReadAt: a.readAt, Stale: !a.loaded || time.Since(a.readAt) > AuthorityStaleAfter}
}

// DesiredGeneration is the newest release ordinal observed in storage; it
// leads the installed release when the newest one failed to install.
func (m *Manager) DesiredGeneration() int64 { return m.desired.Load() }

// HasHardLimits reports whether any key in the pinned authority carries a
// limit the distributed limiter enforces. A limiter outage only degrades
// traffic when such a key exists.
func (m *Manager) HasHardLimits() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, issuer := range m.authority.workloadIssuers {
		if !issuer.Config.Enabled {
			continue
		}
		for _, a := range issuer.bindings {
			if a.Policy.AdmissionLimits().Limited() || a.ProjectEndUserPolicy != nil {
				return true
			}
		}
	}
	if m.authority.installationBudget.Limited() {
		return true
	}
	for _, p := range m.authority.projectAttributionBudgets {
		if len(p) > 0 {
			return true
		}
	}
	for _, p := range m.authority.organizationBudgets {
		if p.Limited() {
			return true
		}
	}
	for _, p := range m.authority.projectBudgets {
		if p.Limited() {
			return true
		}
	}
	for _, record := range m.authority.keys {
		if record.authority.Policy.RouteLimits.Limited() {
			return true
		}
		p := record.authority.Policy
		if p.RequestsPerMinute != nil || p.TokensPerMinute != nil || p.MaxConcurrency != nil ||
			p.DailyCostLimit != nil || p.MonthlyCostLimit != nil || p.WeeklyCostLimit != nil || p.EndUserPolicy != nil || record.authority.ProjectEndUserPolicy != nil ||
			record.authority.BudgetGroupRPM != nil || record.authority.BudgetGroupTPM != nil || record.authority.BudgetGroupConcurrency != nil || record.authority.BudgetGroupDailyCostLimit != nil || record.authority.BudgetGroupMonthlyCostLimit != nil || record.authority.BudgetGroupWeeklyCostLimit != nil {
			return true
		}
	}
	return false
}

func (m *Manager) RoutingInputs() *routinginputs.Inputs {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.inputs
}
func (m *Manager) refreshInputs(ctx context.Context) error {
	now := time.Now()
	current := m.RoutingInputs()
	if current != nil && now.Sub(current.RefreshedAt) < routinginputs.RefreshInterval {
		return nil
	}
	inputs, err := routinginputs.Load(ctx, m.pool, now)
	if err != nil {
		return fmt.Errorf("routing measurements: %w", err)
	}
	m.mu.Lock()
	if m.inputs == nil || inputs.RefreshedAt.After(m.inputs.RefreshedAt) {
		m.inputs = inputs
	}
	m.mu.Unlock()
	return nil
}

// BudgetAuthority gives system-origin work the same aggregate caps and freshness
// bound as caller work, without creating an API-key or end-user identity.
func (m *Manager) BudgetAuthority(project *string) (keyauthority.Authority, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.authority.loaded || time.Since(m.authority.readAt) > AuthorityStaleAfter {
		return keyauthority.Authority{}, ErrStaleAuthority
	}
	a := keyauthority.Authority{BudgetIncreases: m.authority.budgetIncreases, InstallationID: m.installation, ProjectID: project, InstallationBudget: m.authority.installationBudget}
	if project != nil {
		if org, ok := m.authority.projectOrganizations[*project]; ok {
			a.OrganizationID = &org
			a.OrganizationBudget = m.authority.organizationBudgets[org]
		}
		a.ProjectBudget = m.authority.projectBudgets[*project]
		a.ProjectAttributionBudgets = m.authority.projectAttributionBudgets[*project]
	}
	return a, nil
}
