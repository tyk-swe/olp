package release

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/workload"
)

func delegationPrincipals(project, issuer string) (keyauthority.Authority, keyauthority.Authority) {
	return keyauthority.Authority{
		ID: "agent", ProjectID: &project, AgentPrincipal: true, WorkloadIssuerID: &issuer, SnapshotID: "snapshot",
		Policy: keyauthority.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"shared"}},
	}, keyauthority.Authority{
		ID: "key", ProjectID: &project, SnapshotID: "snapshot",
		Policy: keyauthority.KeyPolicy{Scopes: []string{"delegate", "inference"}, AllowedRoutes: []string{"shared"}},
	}
}

func TestDelegationRefusesInvalidBoundsBeforeIssuing(t *testing.T) {
	agent, key := delegationPrincipals("project", "issuer")
	manager := &Manager{}
	for _, test := range []struct {
		name   string
		bounds DelegationBounds
		want   error
	}{
		{name: "default duration", bounds: DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"shared"}}, want: ErrStaleAuthority},
		{name: "maximum duration", bounds: DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"shared"}, TTLSeconds: 900}, want: ErrStaleAuthority},
		{name: "duration exceeds maximum", bounds: DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"shared"}, TTLSeconds: 901}, want: ErrDelegationBounds},
		{name: "negative duration", bounds: DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"shared"}, TTLSeconds: -1}, want: ErrDelegationBounds},
		{name: "missing scope", bounds: DelegationBounds{AllowedRoutes: []string{"shared"}}, want: ErrDelegationBounds},
		{name: "extra scope", bounds: DelegationBounds{Scopes: []string{"inference", "delegate"}, AllowedRoutes: []string{"shared"}}, want: ErrDelegationBounds},
		{name: "wrong scope", bounds: DelegationBounds{Scopes: []string{"tools"}, AllowedRoutes: []string{"shared"}}, want: ErrDelegationBounds},
		{name: "missing routes", bounds: DelegationBounds{Scopes: []string{"inference"}}, want: ErrDelegationBounds},
		{name: "duplicate routes", bounds: DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"shared", "shared"}}, want: ErrDelegationBounds},
		{name: "invalid route", bounds: DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"not a route"}}, want: ErrDelegationBounds},
	} {
		t.Run(test.name, func(t *testing.T) {
			grant, err := manager.CreateDelegation(context.Background(), agent, key, test.bounds)
			if grant.Token != "" || !errors.Is(err, test.want) {
				t.Fatalf("delegation returned grant %+v and error %v, want %v", grant, err, test.want)
			}
		})
	}
}

func TestDelegationRequiresBothPrincipalsAuthority(t *testing.T) {
	project, issuer, otherProject := "project", "issuer", "other"
	agent, key := delegationPrincipals(project, issuer)
	nonAgent := keyauthority.Authority{ProjectID: &project, WorkloadIssuerID: &issuer, SnapshotID: "snapshot"}
	missingIssuer := keyauthority.Authority{ProjectID: &project, AgentPrincipal: true, SnapshotID: "snapshot"}
	otherAgent, _ := delegationPrincipals(otherProject, issuer)
	nestedKey := keyauthority.Authority{ProjectID: &project, WorkloadIssuerID: &issuer, SnapshotID: "snapshot", Policy: key.Policy}
	unprivilegedKey := keyauthority.Authority{ProjectID: &project, SnapshotID: "snapshot", Policy: keyauthority.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"shared"}}}
	staleAgent := keyauthority.Authority{ProjectID: &project, AgentPrincipal: true, WorkloadIssuerID: &issuer, SnapshotID: "old", Policy: agent.Policy}
	restrictedAgent := keyauthority.Authority{ProjectID: &project, AgentPrincipal: true, WorkloadIssuerID: &issuer, SnapshotID: "snapshot", Policy: keyauthority.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"other"}}}
	restrictedKey := keyauthority.Authority{ProjectID: &project, SnapshotID: "snapshot", Policy: keyauthority.KeyPolicy{Scopes: []string{"delegate", "inference"}, AllowedRoutes: []string{"other"}}}
	revokedAt := time.Now()
	revokedKey := keyauthority.Authority{ProjectID: &project, SnapshotID: "snapshot", Policy: key.Policy, RevokedAt: &revokedAt}
	bounds := DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"shared"}}
	manager := &Manager{}
	for _, test := range []struct {
		name  string
		agent keyauthority.Authority
		key   keyauthority.Authority
		stale bool
	}{
		{name: "non-agent workload", agent: nonAgent, key: key},
		{name: "unverified workload", agent: missingIssuer, key: key},
		{name: "different projects", agent: otherAgent, key: key},
		{name: "nested delegation", agent: agent, key: nestedKey},
		{name: "key lacks delegate scope", agent: agent, key: unprivilegedKey},
		{name: "different authority snapshots", agent: staleAgent, key: key, stale: true},
		{name: "agent cannot use route", agent: restrictedAgent, key: key},
		{name: "key cannot use route", agent: agent, key: restrictedKey},
		{name: "revoked key", agent: agent, key: revokedKey},
	} {
		t.Run(test.name, func(t *testing.T) {
			grant, err := manager.CreateDelegation(context.Background(), test.agent, test.key, bounds)
			if grant.Token != "" {
				t.Fatalf("unauthorized delegation returned a token: %+v", grant)
			}
			if test.stale {
				if !errors.Is(err, ErrStaleAuthority) {
					t.Fatalf("stale authority error = %v", err)
				}
				return
			}
			var problem *problem.Problem
			if !errors.As(err, &problem) || problem.Status != 403 || problem.Code != "permission_denied" {
				t.Fatalf("unauthorized delegation error = %v", err)
			}
		})
	}
}

func TestMemberConsentRejectsInvalidAgent(t *testing.T) {
	project, issuer := "project", "issuer"
	now := time.Now()
	active, expired := now.Add(time.Minute), now.Add(-time.Minute)
	agent, _ := delegationPrincipals(project, issuer)
	for _, test := range []struct {
		name  string
		agent keyauthority.Authority
		want  error
	}{
		{name: "missing agent", want: ErrInvalidKey},
		{name: "missing expiry", agent: agent, want: ErrInvalidKey},
		{name: "expired", agent: keyauthority.Authority{ProjectID: &project, AgentPrincipal: true, WorkloadIssuerID: &issuer, ExpiresAt: &expired}, want: ErrInvalidKey},
		{name: "revoked", agent: keyauthority.Authority{ProjectID: &project, AgentPrincipal: true, WorkloadIssuerID: &issuer, ExpiresAt: &active, RevokedAt: &now}, want: ErrInvalidKey},
		{name: "active requires current authority", agent: keyauthority.Authority{ProjectID: &project, AgentPrincipal: true, WorkloadIssuerID: &issuer, SnapshotID: "snapshot", ExpiresAt: &active}, want: ErrStaleAuthority},
	} {
		t.Run(test.name, func(t *testing.T) {
			grant, err := (&Manager{}).ExchangeMemberConsent(context.Background(), test.agent, "consent", DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"shared"}})
			if grant.Token != "" || !errors.Is(err, test.want) {
				t.Fatalf("consent exchange returned grant %+v and error %v, want %v", grant, err, test.want)
			}
		})
	}
}

func TestDelegatingAgentPauseRefusesUnlessRevoked(t *testing.T) {
	project, issuer := "project", "issuer"
	now := time.Now()
	active := now.Add(time.Minute)
	agent := func(revoked, paused *time.Time) *keyauthority.Authority {
		return &keyauthority.Authority{ProjectID: &project, AgentPrincipal: true, WorkloadIssuerID: &issuer, ExpiresAt: &active, RevokedAt: revoked, PausedAt: paused}
	}
	for _, test := range []struct {
		name  string
		agent *keyauthority.Authority
		want  error
	}{
		{name: "active", agent: agent(nil, nil)},
		{name: "paused", agent: agent(nil, &now), want: ErrPausedKey},
		{name: "revoked while paused", agent: agent(&now, &now), want: ErrInvalidKey},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := delegatingAgentRefusal(test.agent, now); !errors.Is(got, test.want) {
				t.Fatalf("delegating agent refusal = %v, want %v", got, test.want)
			}
		})
	}
}

func TestPausedWorkloadAgentCannotReceiveMemberConsent(t *testing.T) {
	project, issuerID, resource := "project", "issuer", "agent"
	now := time.Now()
	agent := keyauthority.Authority{
		ID: "agent", ProjectID: &project, AgentPrincipal: true, WorkloadIssuerID: &issuerID, WorkloadMapping: "worker",
		Policy: keyauthority.KeyPolicy{Scopes: []string{"agents"}, AllowedAgents: []string{resource}},
	}
	key := keyauthority.Authority{
		ID: "key", ProjectID: &project,
		Policy: keyauthority.KeyPolicy{Scopes: []string{"delegate", "agents"}, AllowedAgents: []string{resource}},
	}
	state := authorityState{
		workloadIssuers: map[string]workloadIssuer{
			issuerID: {
				ID:       issuerID,
				Revision: "rev",
				Config: workload.Config{
					Enabled:  true,
					Mappings: []workload.Mapping{{Name: "worker", ProjectID: project, AgentPrincipal: true}},
				},
				bindings: map[string]keyauthority.Authority{"worker": agent},
			},
		},
	}
	paused := agent
	paused.PausedAt = &now
	resolved, _, ok := state.memberConsentAgent(paused)
	if !ok {
		t.Fatal("paused workload agent disappeared from consent resolution")
	}
	if delegationResourcesAllowed(resolved, key, nil, nil, []string{resource}, now) {
		t.Fatal("paused workload agent was allowed to receive member consent")
	}
}

func TestMemberConsentOffersOnlySharedResourcesUnderCurrentAuthority(t *testing.T) {
	project, otherProject, issuerID := "project", "other", "issuer"
	registered := func(id, owner string) keyauthority.Authority {
		return keyauthority.Authority{ID: id, ProjectID: &owner, AgentPrincipal: true, WorkloadIssuerID: &issuerID, WorkloadMapping: id}
	}
	mapping := func(name, owner string) workload.Mapping {
		return workload.Mapping{Name: name, ProjectID: owner, AgentPrincipal: true}
	}
	binding := func(owner string, policy keyauthority.KeyPolicy) keyauthority.Authority {
		return keyauthority.Authority{ProjectID: &owner, AgentPrincipal: true, Policy: policy}
	}
	manager := &Manager{authority: authorityState{
		loaded: true, readAt: time.Now(), id: "current",
		agentPrincipals: map[string]keyauthority.Authority{
			"delegating": {ID: "delegating", ProjectID: &project, Policy: keyauthority.KeyPolicy{
				Scopes: []string{"delegate", "inference", "tools", "agents"}, AllowedRoutes: []string{"alpha", "beta"},
				AllowedToolsets: []string{"toolset"}, AllowedAgents: []string{"target"},
			}},
			"plain": {ID: "plain", ProjectID: &project, Policy: keyauthority.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"alpha"}}},
		},
		workloadPrincipals: map[string]keyauthority.Authority{
			"tooling": registered("tooling", project),
			"router":  registered("router", project),
			"idle":    registered("idle", project),
			"foreign": registered("foreign", otherProject),
		},
		workloadIssuers: map[string]workloadIssuer{issuerID: {
			ID: issuerID, Revision: "rev",
			Config: workload.Config{Enabled: true, Mappings: []workload.Mapping{
				mapping("tooling", project), mapping("router", project), mapping("idle", project), mapping("foreign", otherProject),
			}},
			bindings: map[string]keyauthority.Authority{
				"tooling": binding(project, keyauthority.KeyPolicy{Scopes: []string{"tools", "agents"}, AllowedToolsets: []string{"toolset", "unshared"}, AllowedAgents: []string{"target"}}),
				"router":  binding(project, keyauthority.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"beta", "gamma"}}),
				"idle":    binding(project, keyauthority.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"gamma"}}),
				"foreign": binding(otherProject, keyauthority.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"alpha"}}),
			},
		}},
	}}
	resources := delegatableResources{routes: []string{"alpha", "beta", "gamma"}, toolsets: []string{"toolset", "unshared"}, agents: []string{"target"}}
	options, err := manager.memberConsentOptions("current", project, "delegating", resources)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"id":"router","allowed_routes":["beta"],"allowed_toolsets":[],"allowed_agents":[]},{"id":"tooling","allowed_routes":[],"allowed_toolsets":["toolset"],"allowed_agents":["target"]}]`
	if string(encoded) != want {
		t.Fatalf("consent options = %s, want %s", encoded, want)
	}
	for _, test := range []struct {
		name      string
		authority string
		key       string
		code      string
	}{
		{name: "replaced authority", authority: "replaced", key: "delegating", code: "authority_unavailable"},
		{name: "key without delegate scope", authority: "current", key: "plain", code: "permission_denied"},
	} {
		t.Run(test.name, func(t *testing.T) {
			options, err := manager.memberConsentOptions(test.authority, project, test.key, resources)
			var refusal *problem.Problem
			if options != nil || !errors.As(err, &refusal) || refusal.Code != test.code {
				t.Fatalf("consent options = %v and error %v, want %s", options, err, test.code)
			}
		})
	}
	if target, err := manager.memberConsentTarget("replaced", "router", "delegating"); err == nil {
		t.Fatalf("replaced authority resolved a consent target: %+v", target)
	}
}
