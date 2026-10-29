package runtime

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
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
	delegated := *key.Delegated([]string{"tools"}, nil, []string{toolset}, agent, "", "pair", "grant", time.Now().Add(time.Minute))
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
