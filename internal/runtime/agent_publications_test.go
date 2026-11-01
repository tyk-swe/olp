package runtime

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/workload"
)

func TestConsumerKeyAuthenticationRequiresCurrentExactCredential(t *testing.T) {
	now := time.Now()
	expired, active := now.Add(-time.Minute), now.Add(time.Minute)
	issuer := "workload-issuer"
	auth := secrets.NewAuthKey(make([]byte, 32), "installation")
	secret := "olp_lookup_secret"
	principal := access.Authority{ID: "key", LookupID: "lookup", Policy: access.KeyPolicy{Scopes: []string{"tools"}}}
	for _, test := range []struct {
		name      string
		presented string
		loaded    bool
		readAt    time.Time
		expiresAt *time.Time
		issuerID  *string
		want      error
	}{
		{name: "active", presented: secret, loaded: true, readAt: now, expiresAt: &active},
		{name: "unloaded", presented: secret, readAt: now, want: ErrStaleAuthority},
		{name: "stale", presented: secret, loaded: true, readAt: now.Add(-AuthorityStaleAfter - time.Minute), want: ErrStaleAuthority},
		{name: "missing secret", presented: "olp_lookup", loaded: true, readAt: now, want: ErrInvalidKey},
		{name: "unknown lookup", presented: "olp_other_secret", loaded: true, readAt: now, want: ErrInvalidKey},
		{name: "changed secret", presented: secret + "changed", loaded: true, readAt: now, want: ErrInvalidKey},
		{name: "additional delimiter", presented: secret + "_suffix", loaded: true, readAt: now, want: ErrInvalidKey},
		{name: "expired", presented: secret, loaded: true, readAt: now, expiresAt: &expired, want: ErrInvalidKey},
		{name: "workload identity", presented: secret, loaded: true, readAt: now, issuerID: &issuer, want: ErrInvalidKey},
	} {
		t.Run(test.name, func(t *testing.T) {
			captured := access.Authority{ID: principal.ID, LookupID: principal.LookupID, Policy: principal.Policy, WorkloadIssuerID: test.issuerID}
			manager := Manager{auth: auth, authority: authorityState{
				loaded: test.loaded, readAt: test.readAt,
				keys: map[string]*keyRecord{"lookup": {authority: captured, digest: auth.Digest(secrets.APIKeyDigest, secret), expiresAt: test.expiresAt}},
			}}
			got, err := manager.Authenticate(test.presented)
			if !errors.Is(err, test.want) {
				t.Fatalf("authentication error = %v, want %v", err, test.want)
			}
			if test.want != nil {
				if got != nil {
					t.Fatal("refused authentication returned authority")
				}
				return
			}
			if got == nil || got.ID != principal.ID || !slices.Contains(got.Policy.Scopes, "tools") {
				t.Fatalf("authentication lost the consumer authority: %+v", got)
			}
		})
	}
}

type taskRow struct {
	values []any
	err    error
}

func (r taskRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i := range dest {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(r.values[i]))
	}
	return nil
}

type taskStoreFixture struct {
	row      taskRow
	beginErr error
	tx       *taskTxFixture
}

func (f *taskStoreFixture) QueryRow(context.Context, string, ...any) pgx.Row { return f.row }
func (f *taskStoreFixture) Begin(context.Context) (pgx.Tx, error)            { return f.tx, f.beginErr }

type taskTxFixture struct {
	pgx.Tx
	rows      []taskRow
	commitErr error
}

func (f *taskTxFixture) QueryRow(context.Context, string, ...any) pgx.Row {
	row := f.rows[0]
	f.rows = f.rows[1:]
	return row
}
func (f *taskTxFixture) Commit(context.Context) error   { return f.commitErr }
func (f *taskTxFixture) Rollback(context.Context) error { return nil }
func (f *taskTxFixture) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func TestTaskCapabilityAuthenticatesAndCountsOwnerCallbacks(t *testing.T) {
	project, publication, revision := "project", "agent", "agent-revision"
	now := time.Now()
	expires := now.Add(time.Minute)
	maximum := 2
	definition := agentcontract.Draft{Agent: &agentcontract.Definition{RouteRevisions: []string{"route-revision"}, ToolsetRevisions: []string{"toolset-revision"}, MaxTaskContinuations: &maximum}}
	owner := access.Authority{ID: "owner", ProjectID: &project, Policy: access.KeyPolicy{Scopes: []string{"agents", "inference", "tools"}, AllowedAgents: []string{publication}, AllowedRoutes: []string{"route"}, AllowedToolsets: []string{"toolset"}}}
	newFixture := func() (*Manager, *taskStoreFixture) {
		store := &taskStoreFixture{row: taskRow{values: []any{owner.ID, project, publication, revision, "", (*time.Time)(nil), expires, "", []byte(nil), definition, "", ""}}, tx: &taskTxFixture{rows: []taskRow{{values: []any{"dispatch", "task", definition}}, {values: []any{1}}}}}
		manager := &Manager{auth: secrets.NewAuthKey(make([]byte, 32), "installation"), taskStore: store, authority: authorityState{loaded: true, readAt: now, agentPrincipals: map[string]access.Authority{owner.ID: owner}, agentResources: map[string]agentResourceAuthority{publication: {ProjectID: project, Kind: "agent", Revisions: map[string]bool{revision: true}}}}}
		return manager, store
	}
	t.Run("owner and pinned resources", func(t *testing.T) {
		manager, _ := newFixture()
		a, err := manager.AuthenticateTaskCapability(t.Context(), "olpt_valid")
		if err != nil || a.ID != owner.ID || !slices.Equal(a.TaskCapability.RouteRevisions, []string{"route-revision"}) || !slices.Equal(a.TaskCapability.ToolsetRevisions, []string{"toolset-revision"}) {
			t.Fatalf("callback authority = %+v, error = %v", a, err)
		}
		if err := manager.AdmitTaskCapabilityAttempt(t.Context(), *a, "inference", "route", "route-revision", &project, "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"); err != nil {
			t.Fatalf("owner's pinned model callback refused: %v", err)
		}
	})
	t.Run("workload owner current mapping and expiry", func(t *testing.T) {
		manager, store := newFixture()
		issuerID := "workload-issuer"
		callerExpires := now.Add(30 * time.Second)
		principal := access.Authority{ID: owner.ID, ProjectID: &project, WorkloadIssuerID: &issuerID, WorkloadDigest: "workload-digest", WorkloadMapping: "worker"}
		store.row.values[4], store.row.values[5], store.row.values[7] = "workload-revision", &callerExpires, "bound-user"
		delete(manager.authority.agentPrincipals, owner.ID)
		manager.authority.workloadPrincipals = map[string]access.Authority{principal.WorkloadDigest: principal}
		manager.authority.workloadIssuers = map[string]workloadIssuer{issuerID: {ID: issuerID, Revision: "workload-revision", Config: workload.Config{Enabled: true}, bindings: map[string]access.Authority{"worker": owner}}}
		got, err := manager.AuthenticateTaskCapability(t.Context(), "olpt_valid")
		if err != nil || got.WorkloadDigest != principal.WorkloadDigest || got.EndUserDigest != "bound-user" || got.ExpiresAt == nil || got.ExpiresAt.After(callerExpires) || !got.Allows("inference", "route", &project, now) {
			t.Fatalf("workload callback authority = %+v, error = %v", got, err)
		}
		issuer := manager.authority.workloadIssuers[issuerID]
		issuer.Revision = "new-revision"
		manager.authority.workloadIssuers[issuerID] = issuer
		if _, err := manager.AuthenticateTaskCapability(t.Context(), "olpt_valid"); err == nil {
			t.Fatal("callback survived a workload issuer revision")
		}
	})
	for _, test := range []struct {
		name   string
		change func(*Manager, *taskStoreFixture)
		want   error
	}{
		{name: "unknown or expired capability", change: func(_ *Manager, f *taskStoreFixture) { f.row.err = pgx.ErrNoRows }, want: ErrInvalidKey},
		{name: "store unavailable", change: func(_ *Manager, f *taskStoreFixture) { f.row.err = errors.New("unavailable") }, want: ErrStaleAuthority},
		{name: "delegated caller unavailable", change: func(_ *Manager, f *taskStoreFixture) { f.row.values[8] = []byte("delegation") }, want: ErrInvalidKey},
		{name: "agent definition missing", change: func(_ *Manager, f *taskStoreFixture) { f.row.values[9] = agentcontract.Draft{} }, want: ErrInvalidKey},
		{name: "owner revoked", change: func(m *Manager, _ *taskStoreFixture) {
			revoked := owner
			revoked.RevokedAt = &now
			m.authority.agentPrincipals[owner.ID] = revoked
		}, want: access.Forbidden()},
		{name: "agent retired", change: func(m *Manager, _ *taskStoreFixture) { delete(m.authority.agentResources, publication) }, want: access.Forbidden()},
		{name: "authority stale", change: func(m *Manager, _ *taskStoreFixture) {
			m.authority.readAt = now.Add(-AuthorityStaleAfter - time.Second)
		}, want: ErrStaleAuthority},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, store := newFixture()
			test.change(manager, store)
			if _, err := manager.AuthenticateTaskCapability(t.Context(), "olpt_valid"); !errors.Is(err, test.want) && (test.want == ErrInvalidKey || test.want == ErrStaleAuthority || err == nil) {
				t.Fatalf("authentication error = %v, want %v", err, test.want)
			}
		})
	}
	for _, test := range []struct {
		name     string
		scope    string
		resource string
		revision string
		change   func(*taskStoreFixture)
		want     error
	}{
		{name: "pinned tool", scope: "tools", resource: "toolset", revision: "toolset-revision"},
		{name: "foreign tool revision", scope: "tools", resource: "toolset", revision: "other", want: access.Forbidden()},
		{name: "foreign route revision", scope: "inference", resource: "route", revision: "other", want: access.Forbidden()},
		{name: "unsupported scope", scope: "agents", resource: publication, revision: revision, want: access.Forbidden()},
		{name: "limit exhausted", scope: "tools", resource: "toolset", revision: "toolset-revision", change: func(f *taskStoreFixture) { f.tx.rows[1].err = pgx.ErrNoRows }, want: ErrTaskAttemptLimit},
		{name: "store unavailable", scope: "tools", resource: "toolset", revision: "toolset-revision", change: func(f *taskStoreFixture) { f.beginErr = errors.New("unavailable") }, want: ErrStaleAuthority},
		{name: "task ended", scope: "tools", resource: "toolset", revision: "toolset-revision", change: func(f *taskStoreFixture) { f.tx.rows[0].err = pgx.ErrNoRows }, want: ErrInvalidKey},
		{name: "commit failed", scope: "tools", resource: "toolset", revision: "toolset-revision", change: func(f *taskStoreFixture) { f.tx.commitErr = errors.New("unavailable") }, want: ErrStaleAuthority},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, store := newFixture()
			a, err := manager.AuthenticateTaskCapability(t.Context(), "olpt_valid")
			if err != nil {
				t.Fatal(err)
			}
			if test.change != nil {
				test.change(store)
			}
			err = manager.AdmitTaskCapabilityAttempt(t.Context(), *a, test.scope, test.resource, test.revision, &project, "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607")
			if test.want == nil && err != nil || test.want != nil && err == nil || (test.want == ErrTaskAttemptLimit || test.want == ErrStaleAuthority || test.want == ErrInvalidKey) && !errors.Is(err, test.want) {
				t.Fatalf("admission error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestAgentLiveAuthority(t *testing.T) {
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	resource := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"
	a := access.Authority{ID: project, ProjectID: &project, Policy: access.KeyPolicy{Scopes: []string{"tools"}, AllowedToolsets: []string{resource}}}
	m := Manager{authority: authorityState{loaded: true, readAt: time.Now(), agentPrincipals: map[string]access.Authority{a.ID: a}, agentResources: map[string]agentResourceAuthority{resource: {ProjectID: project, Kind: "toolset", Revisions: map[string]bool{resource: true}}}}}
	if err := m.AuthorizeAgentResource(a, "tools", resource, resource, project); err != nil {
		t.Fatal(err)
	}
	delete(m.authority.agentResources, resource)
	if m.AuthorizeAgentResource(a, "tools", resource, resource, project) == nil {
		t.Fatal("installed revision overrode live retirement")
	}
	m.authority.agentResources[resource] = agentResourceAuthority{ProjectID: project, Kind: "toolset", Revisions: map[string]bool{resource: true}}
	live := a
	live.Policy.AllowedToolsets = nil
	m.authority.agentPrincipals[a.ID] = live
	if m.AuthorizeAgentResource(a, "tools", resource, resource, project) == nil {
		t.Fatal("captured key authority overrode live revocation")
	}
	m.authority.agentPrincipals[a.ID] = a
	m.authority.readAt = time.Now().Add(-AuthorityStaleAfter - time.Second)
	if m.AuthorizeAgentResource(a, "tools", resource, resource, project) == nil {
		t.Fatal("stale live authority accepted")
	}
}

func TestAgentWorkloadLiveAuthority(t *testing.T) {
	project, resource, issuerID := "project", "resource", "issuer"
	for _, scope := range []string{"tools", "agents"} {
		t.Run(scope, func(t *testing.T) {
			a := access.Authority{ID: "principal", ProjectID: &project, WorkloadIssuerID: &issuerID, WorkloadDigest: "digest", WorkloadMapping: "worker", WorkloadRevision: "revision", Policy: access.KeyPolicy{Scopes: []string{scope}, AllowedToolsets: []string{resource}, AllowedAgents: []string{resource}}}
			kind := "toolset"
			if scope == "agents" {
				kind = "agent"
			}
			for _, scenario := range []string{"allowed", "new principal", "disabled issuer", "removed issuer", "changed revision", "removed mapping", "reduced permissions", "revoked principal", "changed project"} {
				t.Run(scenario, func(t *testing.T) {
					principal := a
					issuer := workloadIssuer{ID: issuerID, Revision: a.WorkloadRevision, Config: workload.Config{Enabled: true}, bindings: map[string]access.Authority{a.WorkloadMapping: a}}
					switch scenario {
					case "new principal":
						principal.Policy = access.KeyPolicy{} // Enrollment caches revocation metadata only.
					case "disabled issuer":
						issuer.Config.Enabled = false
					case "changed revision":
						issuer.Revision = "new revision"
					case "removed mapping":
						delete(issuer.bindings, a.WorkloadMapping)
					case "reduced permissions":
						live := a
						live.Policy = access.KeyPolicy{Scopes: []string{scope}}
						issuer.bindings[a.WorkloadMapping] = live
					case "revoked principal":
						now := time.Now()
						principal.RevokedAt = &now
					case "changed project":
						other := "other project"
						live := a
						live.ProjectID = &other
						issuer.bindings[a.WorkloadMapping] = live
					}
					m := Manager{authority: authorityState{loaded: true, readAt: time.Now(), workloadPrincipals: map[string]access.Authority{a.WorkloadDigest: principal}, workloadIssuers: map[string]workloadIssuer{"issuer": issuer}, agentResources: map[string]agentResourceAuthority{resource: {ProjectID: project, Kind: kind, Revisions: map[string]bool{resource: true}}}}}
					if scenario == "removed issuer" {
						delete(m.authority.workloadIssuers, "issuer")
					}
					err := m.AuthorizeAgentResource(a, scope, resource, resource, project)
					wantAllowed := scenario == "allowed" || scenario == "new principal"
					if (err == nil) != wantAllowed {
						t.Fatalf("allowed=%t, want %t: %v", err == nil, wantAllowed, err)
					}
				})
			}
		})
	}
}

func TestAgentTaskRechecksWorkloadRevisionAndExpiry(t *testing.T) {
	project, resource, issuerID := "project", "resource", "issuer"
	now := time.Now()
	active, expired := now.Add(time.Minute), now.Add(-time.Minute)
	principal := access.Authority{ID: "principal", ProjectID: &project, WorkloadIssuerID: &issuerID, WorkloadDigest: "digest", WorkloadMapping: "worker"}
	binding := access.Authority{ID: principal.ID, ProjectID: &project, Policy: access.KeyPolicy{Scopes: []string{"agents"}, AllowedAgents: []string{resource}}}
	manager := Manager{authority: authorityState{
		loaded:             true,
		readAt:             now,
		workloadPrincipals: map[string]access.Authority{principal.WorkloadDigest: principal},
		workloadIssuers:    map[string]workloadIssuer{issuerID: {ID: issuerID, Revision: "revision", Config: workload.Config{Enabled: true}, bindings: map[string]access.Authority{principal.WorkloadMapping: binding}}},
		agentResources:     map[string]agentResourceAuthority{resource: {ProjectID: project, Kind: "agent", Revisions: map[string]bool{resource: true}}},
	}}
	for _, test := range []struct {
		name     string
		revision string
		expiry   *time.Time
		allowed  bool
	}{
		{name: "current assertion", revision: "revision", expiry: &active, allowed: true},
		{name: "replaced issuer revision", revision: "old", expiry: &active},
		{name: "expired assertion", revision: "revision", expiry: &expired},
		{name: "unknown expiry", revision: "revision"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := manager.AuthorizeAgentTask(principal.ID, project, resource, resource, test.revision, test.expiry)
			if (err == nil) != test.allowed {
				t.Fatalf("task authorization error = %v, allowed = %t", err, test.allowed)
			}
		})
	}
}

func TestMCPToolAuthorizationUsesCurrentPublicationAndServerRevision(t *testing.T) {
	project, publication, revision, server, serverRevision := "project", "tools", "revision", "server", "server-revision"
	now := time.Now()
	caller := access.Authority{ID: "principal", ProjectID: &project, Policy: access.KeyPolicy{Scopes: []string{"tools"}, AllowedToolsets: []string{publication}}}
	noScope := access.Authority{ID: caller.ID, ProjectID: &project, Policy: access.KeyPolicy{AllowedToolsets: []string{publication}}}
	revoked := access.Authority{ID: caller.ID, ProjectID: &project, Policy: caller.Policy, RevokedAt: &now}
	published := map[string]mcpToolAuthority{revision: {PublicationID: publication, ProjectID: project, Servers: map[string]string{server: serverRevision}}}
	wrongPublication := map[string]mcpToolAuthority{revision: {PublicationID: "other", ProjectID: project, Servers: map[string]string{server: serverRevision}}}
	wrongProject := map[string]mcpToolAuthority{revision: {PublicationID: publication, ProjectID: "other", Servers: map[string]string{server: serverRevision}}}
	for _, test := range []struct {
		name             string
		caller           access.Authority
		current          access.Authority
		toolsets         map[string]mcpToolAuthority
		readAt           time.Time
		requestedID      string
		requestedRev     string
		requestedHost    string
		requestedHostRev string
		allowed          bool
		stale            bool
	}{
		{name: "current server", caller: caller, current: caller, toolsets: published, readAt: now, requestedID: publication, requestedRev: revision, requestedHost: server, requestedHostRev: serverRevision, allowed: true},
		{name: "published toolset discovery", caller: caller, current: caller, toolsets: published, readAt: now, requestedID: publication, requestedRev: revision, allowed: true},
		{name: "caller lacks scope", caller: noScope, current: caller, toolsets: published, readAt: now, requestedID: publication, requestedRev: revision, requestedHost: server, requestedHostRev: serverRevision},
		{name: "principal revoked", caller: caller, current: revoked, toolsets: published, readAt: now, requestedID: publication, requestedRev: revision, requestedHost: server, requestedHostRev: serverRevision},
		{name: "publication retired", caller: caller, current: caller, readAt: now, requestedID: publication, requestedRev: revision, requestedHost: server, requestedHostRev: serverRevision},
		{name: "different publication", caller: caller, current: caller, toolsets: wrongPublication, readAt: now, requestedID: publication, requestedRev: revision, requestedHost: server, requestedHostRev: serverRevision},
		{name: "different project", caller: caller, current: caller, toolsets: wrongProject, readAt: now, requestedID: publication, requestedRev: revision, requestedHost: server, requestedHostRev: serverRevision},
		{name: "server revision changed", caller: caller, current: caller, toolsets: published, readAt: now, requestedID: publication, requestedRev: revision, requestedHost: server, requestedHostRev: "old"},
		{name: "server removed", caller: caller, current: caller, toolsets: published, readAt: now, requestedID: publication, requestedRev: revision, requestedHost: "other", requestedHostRev: serverRevision},
		{name: "stale authority", caller: caller, current: caller, toolsets: published, readAt: now.Add(-AuthorityStaleAfter - time.Second), requestedID: publication, requestedRev: revision, requestedHost: server, requestedHostRev: serverRevision, stale: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := Manager{authority: authorityState{loaded: true, readAt: test.readAt, agentPrincipals: map[string]access.Authority{caller.ID: test.current}, mcpTools: test.toolsets}}
			err := manager.AuthorizeMCPTool(test.caller, test.requestedID, test.requestedRev, project, test.requestedHost, test.requestedHostRev)
			if test.stale {
				if !errors.Is(err, ErrStaleAuthority) {
					t.Fatalf("stale authority error = %v", err)
				}
				return
			}
			if (err == nil) != test.allowed {
				t.Fatalf("authorization allowed = %t, want %t: %v", err == nil, test.allowed, err)
			}
		})
	}
}

func TestDelegatedToolAuthorityRechecksAgent(t *testing.T) {
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	toolset := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"
	issuerID := "issuer"
	digest := "workload"
	agent := access.Authority{ID: "agent", AgentPrincipal: true, ProjectID: &project, WorkloadIssuerID: &issuerID, WorkloadDigest: digest, WorkloadRevision: "revision", WorkloadMapping: "mapping", Policy: access.KeyPolicy{Scopes: []string{"tools"}, AllowedToolsets: []string{toolset}}}
	key := access.Authority{ID: "key", ProjectID: &project, Policy: access.KeyPolicy{Scopes: []string{"tools"}, AllowedToolsets: []string{toolset}}}
	delegated := *key.Delegated([]string{"tools"}, nil, []string{toolset}, nil, agent, "", "pair", "grant", time.Now().Add(time.Minute))
	for _, test := range []struct {
		name     string
		agent    access.Authority
		enabled  bool
		revision string
		allowed  bool
	}{
		{name: "allowed", agent: agent, enabled: true, revision: "revision", allowed: true},
		{name: "issuer disabled", agent: agent, revision: "revision"},
		{name: "issuer revised", agent: agent, enabled: true, revision: "other"},
		{name: "agent loses toolset", agent: access.Authority{ID: agent.ID, ProjectID: &project, WorkloadIssuerID: &issuerID, WorkloadMapping: "mapping", Policy: access.KeyPolicy{Scopes: []string{"tools"}}}, enabled: true, revision: "revision"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := Manager{authority: authorityState{loaded: true, readAt: time.Now(), agentPrincipals: map[string]access.Authority{key.ID: key}, workloadPrincipals: map[string]access.Authority{digest: agent}, workloadIssuers: map[string]workloadIssuer{"issuer": {ID: issuerID, Revision: test.revision, Config: workload.Config{Enabled: test.enabled}, bindings: map[string]access.Authority{"mapping": test.agent}}}, mcpTools: map[string]mcpToolAuthority{toolset: {PublicationID: toolset, ProjectID: project}}}}
			err := manager.AuthorizeMCPTool(delegated, toolset, toolset, project, "", "")
			if (err == nil) != test.allowed {
				t.Fatalf("authorization allowed = %t, want %t: %v", err == nil, test.allowed, err)
			}
		})
	}
}
