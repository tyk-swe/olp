package runtime

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/secretstore"
	"github.com/tyk-swe/olp/internal/usage"
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
)

// Release is one installed snapshot pinned for the lifetime of a request.
type Release struct {
	ID          string
	Sequence    int64
	Digest      string
	Snapshot    *routing.Snapshot
	InstalledAt time.Time
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

// NewRelease freezes an independent serving snapshot and credentials. The
// caller retains its mutable inputs; serving consumers borrow the resulting
// release and must not mutate it. Database installation already decodes into
// exclusively owned storage and does not pay for this construction copy.
func NewRelease(id string, sequence int64, snapshot *routing.Snapshot, credentials map[string][]byte) (*Release, error) {
	if snapshot == nil {
		return nil, errors.New("snapshot is required")
	}
	frozen := snapshot.Clone()
	if err := frozen.Compile(); err != nil {
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
	return &Release{ID: id, Sequence: sequence, Digest: digest, Snapshot: frozen, InstalledAt: time.Now(), credentials: owned}, nil
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
	authority access.Authority
	digest    []byte
}

type authorityState struct {
	mcpTools                  map[string]mcpToolAuthority
	agentPrincipals           map[string]access.Authority
	agentResources            map[string]agentResourceAuthority
	workloadIssuers           map[string]workloadIssuer
	workloadPrincipals        map[string]access.Authority
	budgetIncreases           map[string]string
	installationBudget        *access.BudgetPolicy
	projectOrganizations      map[string]string
	organizationBudgets       map[string]*access.BudgetPolicy
	projectBudgets            map[string]*access.BudgetPolicy
	projectAttributionBudgets map[string]access.AttributionBudgets
	loaded                    bool
	readAt                    time.Time
	id                        string
	sequence                  int64
	keys                      map[string]*keyRecord
	// ineligible holds the credential versions and network credentials that
	// may not serve, with why.
	ineligible map[string]routing.Eligibility
}

// Manager installs releases and refreshes key authority for one gateway.
type Manager struct {
	PrepareGuardrailPlugin func(context.Context, string) (func(), error)
	workloadKeys           *workload.Cache
	workloadRegistration   sync.Mutex
	pool                   *pgxpool.Pool
	// ReadPool, when set before Start, supplies runtime release and authority
	// reads. Writes and historical secret resolution always use the primary.
	ReadPool *pgxpool.Pool
	// Region is the deployment's explicit region name, fixed before Start.
	Region string
	// ExternalSecrets is fixed before Start and follows the provider egress policy.
	ExternalSecrets  ExternalSecrets
	external         externalCredentials
	authorityRefresh sync.Mutex
	grantRefresh     sync.Mutex
	replica          replicaProgress
	installation     string
	auth             *secrets.AuthKey
	keys             *secrets.KeyRing
	Mounted          map[string]MountedProvider
	log              *slog.Logger
	InstanceID       string

	// GrantRefreshed, when set before Start, is told of each credential
	// version of the installed release whose grant a poll found refreshed,
	// with the provider it belongs to, apart from the poll.
	GrantRefreshed func(providerID, credentialID string)

	mu        sync.RWMutex
	taskStore taskCapabilityStore
	authority authorityState
	release   *Release
	failed    int64
	inputs    *usage.RoutingInputs
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
	stopping   bool
	waitStop   sync.Once
	stopped    chan struct{}
}

// NewManager prepares a manager that serves the empty snapshot until the
// first release installs and rejects admissions until authority loads.
func NewManager(pool *pgxpool.Pool, installation string, auth *secrets.AuthKey, keys *secrets.KeyRing, log *slog.Logger) *Manager {
	tasks, cancel := context.WithCancel(context.Background())
	return &Manager{workloadKeys: workload.NewCache(access.IdentityHTTPClient()),
		pool:         pool,
		installation: installation,
		auth:         auth,
		keys:         keys,
		log:          log,
		release:      emptyRelease(),
		stop:         make(chan struct{}),
		taskCtx:      tasks,
		cancelTask:   cancel,
		stopped:      make(chan struct{}),
		InstanceID:   usage.GatewayInstance(),

		refreshRequested: map[string]int64{},
	}
}

func emptyRelease() *Release {
	return &Release{Snapshot: &routing.Snapshot{Providers: map[string]routing.Provider{}, Routes: map[string]routing.Route{}}, InstalledAt: time.Now()}
}

// Start performs one synchronous refresh and then polls until ctx ends or
// Stop is called. Refresh failures are logged; the gateway keeps serving its
// last installed state and fails closed once authority goes stale.
func (m *Manager) Start(ctx context.Context) {
	m.tasksMu.Lock()
	if m.stopping {
		m.tasksMu.Unlock()
		return
	}
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

// Stop ends polling and waits for the loop to exit.
func (m *Manager) Stop() {
	_ = m.StopContext(context.Background())
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
	return errors.Join(err, m.RecordInstallStatus(status, m.InstanceID))
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

func (m *Manager) refreshAuthority(ctx context.Context) error {
	m.authorityRefresh.Lock()
	defer m.authorityRefresh.Unlock()
	start, err := m.authorityReadTime(ctx)
	if err != nil {
		return fmt.Errorf("authority replica freshness: %w", err)
	}
	tx, err := m.runtimeReadPool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	defer tx.Rollback(ctx)
	var id, installation string
	var sequence int64
	if err = tx.QueryRow(ctx, "SELECT id::text,COALESCE(authority_id::text,''),authority_sequence FROM olp.installation WHERE singleton").Scan(&installation, &id, &sequence); err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	if installation != m.installation {
		return errors.New("runtime read database belongs to another installation")
	}
	m.mu.Lock()
	if m.authority.loaded && m.authority.id == id && sequence < m.authority.sequence {
		m.mu.Unlock()
		return ErrStaleAuthority
	}
	unchanged := m.authority.loaded && m.authority.id == id && m.authority.sequence == sequence
	if unchanged {
		m.authority.readAt = start
	}
	m.mu.Unlock()
	if unchanged {
		return nil
	}
	state := authorityState{loaded: true, readAt: start, id: id, sequence: sequence, keys: map[string]*keyRecord{}}
	if err = tx.QueryRow(ctx, "SELECT budget_policy FROM olp.installation WHERE singleton").Scan(&state.installationBudget); err != nil {
		return err
	}
	state.budgetIncreases, err = limits.LoadBudgetIncreases(ctx, tx)
	if err != nil {
		return err
	}
	budgetRows, err := tx.Query(ctx, "SELECT p.id::text,p.budget_policy,p.attribution_budgets,p.organization_id::text,o.budget_policy FROM olp.projects p LEFT JOIN olp.organizations o ON o.id=p.organization_id WHERE p.organization_id IS NOT NULL OR p.budget_policy IS NOT NULL OR p.attribution_budgets<>'{}'::jsonb")
	if err != nil {
		return err
	}
	state.projectOrganizations = map[string]string{}
	state.organizationBudgets = map[string]*access.BudgetPolicy{}
	state.projectBudgets = map[string]*access.BudgetPolicy{}
	state.projectAttributionBudgets = map[string]access.AttributionBudgets{}
	for budgetRows.Next() {
		var project string
		var policy *access.BudgetPolicy
		var labels access.AttributionBudgets
		var organization *string
		var organizationPolicy *access.BudgetPolicy
		if err = budgetRows.Scan(&project, &policy, &labels, &organization, &organizationPolicy); err != nil {
			budgetRows.Close()
			return err
		}
		if organization != nil {
			state.projectOrganizations[project] = *organization
			state.organizationBudgets[*organization] = organizationPolicy
		}
		state.projectBudgets[project] = policy
		state.projectAttributionBudgets[project] = labels
	}
	budgetRows.Close()
	if err = budgetRows.Err(); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, "SELECT k.id::text,k.lookup_id,k.created_by::text,k.project_id::text,k.digest,k.policy,k.expires_at,k.revoked_at,k.budget_group_id::text,g.daily_cost_limit::text,g.monthly_cost_limit::text,g.weekly_cost_limit::text,p.end_user_policy,COALESCE(p.attribution_policy,'{}'::jsonb),COALESCE(k.limit_lookup_id,k.lookup_id),g.limit_template,g.requests_per_minute,g.tokens_per_minute,g.max_concurrency,COALESCE(g.cost_basis,'cost'),k.workload_issuer_id::text,COALESCE(k.workload_digest,''),COALESCE(k.workload_mapping,'') FROM olp.api_keys k LEFT JOIN olp.budget_groups g ON g.id=k.budget_group_id LEFT JOIN olp.projects p ON p.id=k.project_id")
	if err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	for rows.Next() {
		var policy []byte
		record := keyRecord{}
		if err = rows.Scan(&record.authority.ID, &record.authority.LookupID, &record.authority.Issuer, &record.authority.ProjectID, &record.digest, &policy, &record.authority.ExpiresAt, &record.authority.RevokedAt, &record.authority.BudgetGroupID, &record.authority.BudgetGroupDailyCostLimit, &record.authority.BudgetGroupMonthlyCostLimit, &record.authority.BudgetGroupWeeklyCostLimit, &record.authority.ProjectEndUserPolicy, &record.authority.ProjectAttributionPolicy, &record.authority.LimitLookupID, &record.authority.BudgetGroupTemplate, &record.authority.BudgetGroupRPM, &record.authority.BudgetGroupTPM, &record.authority.BudgetGroupConcurrency, &record.authority.BudgetGroupCostBasis, &record.authority.WorkloadIssuerID, &record.authority.WorkloadDigest, &record.authority.WorkloadMapping); err != nil {
			rows.Close()
			return fmt.Errorf("authority: %w", err)
		}
		if err = json.Unmarshal(policy, &record.authority.Policy); err != nil {
			rows.Close()
			return fmt.Errorf("authority: key %s policy: %w", record.authority.ID, err)
		}
		record.authority.SnapshotID = state.id
		record.authority.Policy = record.authority.Policy.InRegion(m.Region)
		record.authority.BudgetIncreases = state.budgetIncreases
		record.authority.InstallationID = m.installation
		record.authority.InstallationBudget = state.installationBudget
		if record.authority.ProjectID != nil {
			if org, ok := state.projectOrganizations[*record.authority.ProjectID]; ok {
				record.authority.OrganizationID = &org
				record.authority.OrganizationBudget = state.organizationBudgets[org]
			}
			record.authority.ProjectBudget = state.projectBudgets[*record.authority.ProjectID]
			record.authority.ProjectAttributionBudgets = state.projectAttributionBudgets[*record.authority.ProjectID]
		}
		state.keys[record.authority.LookupID] = &record
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return fmt.Errorf("authority: %w", err)
	}

	if err = m.loadWorkloadAuthority(ctx, tx, &state); err != nil {
		return err
	}
	// Fetch each needed project's group document once, rather than copying all
	// groups into every key row (including keys which never reference a group).
	var projectIDs []string
	var needed map[string]bool
	for _, record := range state.keys {
		a := record.authority
		if a.WorkloadIssuerID != nil {
			continue
		} // Workload bindings are compiled from current issuer policy.
		if (len(a.Policy.AllowedRouteGroups) == 0 && !a.UsesLimitTemplates()) || a.ProjectID == nil {
			continue
		}
		if needed == nil {
			needed = map[string]bool{}
		}
		if !needed[*a.ProjectID] {
			needed[*a.ProjectID] = true
			projectIDs = append(projectIDs, *a.ProjectID)
		}
	}
	if len(projectIDs) > 0 {
		groupRows, e := tx.Query(ctx, `SELECT id::text,route_groups,limit_templates FROM olp.projects WHERE id=ANY($1::uuid[])`, projectIDs)
		if e != nil {
			return fmt.Errorf("authority route groups: %w", e)
		}
		byProject := map[string]access.RouteGroups{}
		templatesByProject := map[string]access.LimitTemplates{}
		for groupRows.Next() {
			var id string
			var groups access.RouteGroups
			var templates access.LimitTemplates
			if e = groupRows.Scan(&id, &groups, &templates); e != nil {
				groupRows.Close()
				return e
			}
			byProject[id] = groups
			templatesByProject[id] = templates
		}
		groupRows.Close()
		if e = groupRows.Err(); e != nil {
			return e
		}
		for _, record := range state.keys {
			if record.authority.ProjectID == nil || record.authority.WorkloadIssuerID != nil {
				continue
			}
			if e = record.authority.BindRouteGroups(byProject[*record.authority.ProjectID]); e != nil {
				return fmt.Errorf("authority route groups: %w", e)
			}
			// A revoked key never admits, and its template may since have gone.
			if record.authority.RevokedAt != nil {
				continue
			}
			if e = record.authority.BindLimitTemplates(templatesByProject[*record.authority.ProjectID]); e != nil {
				return e
			}
		}
	}
	state.workloadPrincipals = map[string]access.Authority{}
	for _, record := range state.keys {
		if record.authority.WorkloadIssuerID != nil {
			state.workloadPrincipals[record.authority.WorkloadDigest] = record.authority
		}
	}
	if err = m.loadKeyOverlaps(ctx, tx, &state); err != nil {
		return fmt.Errorf("authority overlaps: %w", err)
	}
	if state.ineligible, err = ReadIneligible(ctx, tx, nil); err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	state.agentPrincipals = map[string]access.Authority{}
	for _, record := range state.keys {
		state.agentPrincipals[record.authority.ID] = record.authority
	}
	if state.agentResources, err = loadAgentResourceAuthority(ctx, tx); err != nil {
		return err
	}
	if state.mcpTools, err = loadMCPToolAuthority(ctx, tx); err != nil {
		return err
	}
	m.mu.Lock()
	m.authority = state
	m.mu.Unlock()
	m.log.Info("authority refreshed", "sequence", sequence, "keys", len(state.keys), "ineligible_credentials", len(state.ineligible))
	return nil
}

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
		m.log.Info("release installed", "sequence", sequence, "sha256", digest, "providers", len(release.Snapshot.Providers), "routes", len(release.Snapshot.Routes))
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
	release := &Release{ID: id, Sequence: sequence, Digest: digest, Snapshot: snapshot, InstalledAt: time.Now(), credentials: map[string][]byte{}}
	m.mu.RLock()
	candidate := &releaseCandidate{release: release, baseSequence: m.release.Sequence}
	knownGrants := m.grants
	m.mu.RUnlock()
	if m.keys == nil {
		credentials, err := installMounted(snapshot, m.Mounted)
		if err != nil {
			return nil, err
		}
		release.credentials = credentials
		return candidate, nil
	}
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var credentialIDs []string
	for _, provider := range snapshot.Providers {
		for _, slot := range provider.Slots {
			if slot.CredentialID != nil {
				credentialIDs = append(credentialIDs, *slot.CredentialID)
			}
		}
	}
	release.references, err = readExternalReferences(ctx, tx, credentialIDs)
	if err != nil {
		return nil, err
	}
	for _, provider := range snapshot.Providers {
		if provider.Network != nil && provider.Network.CredentialID != "" {
			id := provider.Network.CredentialID
			var owner string
			if err := tx.QueryRow(ctx, "SELECT provider_id::text FROM olp.provider_network_credentials WHERE id=$1", id).Scan(&owner); err != nil || owner != provider.ID {
				return nil, fmt.Errorf("provider %s network credential unavailable", provider.ID)
			}
			secret, err := m.keys.Read(ctx, tx, m.installation, id, secrets.ProviderCredential)
			if err != nil {
				return nil, fmt.Errorf("provider %s network credential unavailable", provider.ID)
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
				return nil, fmt.Errorf("credential %s for provider %s: %w", *slot.CredentialID, provider.ID, err)
			}
			release.credentials[*slot.CredentialID] = secret
		}
	}
	if snapshot.Guardrails != nil {
		pins := &guardrailPluginPins{}
		goruntime.AddCleanup(release, func(p *guardrailPluginPins) { p.close() }, pins)
		pinned := map[string]bool{}
		for _, definition := range snapshot.Guardrails.Definitions {
			if definition.Type == "plugin" {
				var c guardrails.ExternalConfig
				if json.Unmarshal(definition.Configuration, &c) != nil || m.PrepareGuardrailPlugin == nil {
					return nil, errors.New("guardrail plugin unavailable")
				}
				if !pinned[c.PluginVersion] {
					unpin, err := m.PrepareGuardrailPlugin(ctx, c.PluginVersion)
					if err != nil {
						return nil, errors.New("guardrail plugin could not be preloaded")
					}
					pins.add(unpin)
					pinned[c.PluginVersion] = true
				}
			}
			if !strings.HasPrefix(definition.Type, "builtin.") {
				var config guardrails.ExternalConfig
				if err := json.Unmarshal(definition.Configuration, &config); err != nil {
					return nil, errors.New("invalid guardrail configuration")
				}
				ids := []string{config.CredentialRef}
				if config.Network != nil {
					ids = append(ids, config.Network.CredentialID)
				}
				for _, id := range ids {
					if id == "" {
						continue
					}
					var project string
					if err := tx.QueryRow(ctx, "SELECT project_id::text FROM olp.guardrail_credentials WHERE id=$1", id).Scan(&project); err != nil || project != definition.ProjectID {
						return nil, errors.New("guardrail credential boundary mismatch")
					}
					secret, err := m.keys.Read(ctx, tx, m.installation, id, secrets.GuardrailCredential)
					if err != nil {
						return nil, errors.New("guardrail credential unavailable")
					}
					release.credentials[id] = secret
				}
			}
		}
	}
	// A newly published grant credential must already have a coherent token
	// and generation. Requests can then gate and dispatch against the same
	// generation, and later polls can observe its advancement.
	candidate.grants, err = m.readGrants(ctx, tx, grantProviders(snapshot), knownGrants)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	// Record store availability before publication so failed references produce
	// explicit route-plan decisions. Fetches hold neither transaction nor mutex.
	for id, reference := range release.references {
		value, _ := m.external.read(ctx, m.ExternalSecrets, id, reference)
		clear(value)
	}
	return candidate, nil
}

// publishRelease advances release identity and its initial grant membership
// together. Grant polls keep making progress while installation performs remote
// I/O, but cannot publish old membership after this transition. A seed read
// before a concurrent rotation never replaces a newer cached token.
func (m *Manager) publishRelease(ctx context.Context, candidate *releaseCandidate) (bool, error) {
	m.grantRefresh.Lock()
	defer m.grantRefresh.Unlock()
	m.mu.Lock()
	// Waiting for a poll must not let cancelled or expired installation work
	// publish afterward. The last-good release remains owned by the manager.
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return false, err
	}
	if candidate.release.Sequence <= m.release.Sequence {
		m.mu.Unlock()
		return false, nil
	}
	// An intervening release may have pruned a newer observed generation of
	// a credential this candidate brings back. Retry preparation against that
	// release instead of resurrecting an older token or retaining old secrets.
	if len(candidate.grants) > 0 && candidate.baseSequence != m.release.Sequence {
		m.mu.Unlock()
		return false, nil
	}
	for credentialID, previous := range m.grants {
		if seeded, ok := candidate.grants[credentialID]; ok && previous.generation >= seeded.generation {
			candidate.grants[credentialID] = previous
		}
	}
	refreshed := advancedGrants(grantProviders(candidate.release.Snapshot), m.grants, candidate.grants)
	m.serveGrantsLocked(candidate.grants)
	m.release = candidate.release
	m.mu.Unlock()
	m.grantsRefreshed(refreshed)
	return true, nil
}

// Release borrows the currently installed immutable release for pinning. Its
// snapshot, maps, slices, nested pointers, and secret storage must not be mutated.
// Mutable planning/request state belongs in the consumer, outside the release.
func (m *Manager) Release() *Release {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.release
}

// EndUserDigest derives a project-scoped identity without retaining the identifier.
func (m *Manager) EndUserDigest(projectID *string, identifier string) string {
	return access.DigestEndUser(m.auth, projectID, identifier)
}

func (m *Manager) Digest(purpose secrets.DigestPurpose, value string) []byte {
	return m.auth.Digest(purpose, value)
}

// Authenticate resolves an API key against the last authority read. The
// authority it returns is shared with the installed key records: callers read
// it and copy it before changing it.
func (m *Manager) Authenticate(secret string) (*access.Authority, error) {
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

func (m *Manager) RoutingInputs() *usage.RoutingInputs {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.inputs
}
func (m *Manager) refreshInputs(ctx context.Context) error {
	now := time.Now()
	current := m.RoutingInputs()
	if current != nil && now.Sub(current.RefreshedAt) < usage.RoutingRefreshInterval {
		return nil
	}
	inputs, err := usage.LoadRoutingInputs(ctx, m.pool, now)
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

func (m *Manager) loadKeyOverlaps(ctx context.Context, tx pgx.Tx, state *authorityState) error {
	rows, err := tx.Query(ctx, "SELECT api_key_id::text,lookup_id,digest,expires_at FROM olp.api_key_overlaps WHERE expires_at>now()")
	if err != nil {
		return err
	}
	defer rows.Close()
	var byID map[string]*keyRecord
	for rows.Next() {
		var id, lookup string
		var digest []byte
		var until time.Time
		if err = rows.Scan(&id, &lookup, &digest, &until); err != nil {
			return err
		}
		if byID == nil {
			byID = make(map[string]*keyRecord, len(state.keys))
			for _, r := range state.keys {
				byID[r.authority.ID] = r
			}
		}
		if record, ok := byID[id]; ok {
			overlap := *record
			overlap.digest = digest
			overlap.expiresAt = &until
			state.keys[lookup] = &overlap
		}
	}
	return rows.Err()
}

// BudgetAuthority gives system-origin work the same aggregate caps and freshness
// bound as caller work, without creating an API-key or end-user identity.
func (m *Manager) BudgetAuthority(project *string) (access.Authority, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.authority.loaded || time.Since(m.authority.readAt) > AuthorityStaleAfter {
		return access.Authority{}, ErrStaleAuthority
	}
	a := access.Authority{BudgetIncreases: m.authority.budgetIncreases, InstallationID: m.installation, ProjectID: project, InstallationBudget: m.authority.installationBudget}
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
