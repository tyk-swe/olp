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
