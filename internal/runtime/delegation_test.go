package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

func delegationPrincipals(project, issuer string) (access.Authority, access.Authority) {
	return access.Authority{
		ID: "agent", ProjectID: &project, AgentPrincipal: true, WorkloadIssuerID: &issuer, SnapshotID: "snapshot",
		Policy: access.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"shared"}},
	}, access.Authority{
		ID: "key", ProjectID: &project, SnapshotID: "snapshot",
		Policy: access.KeyPolicy{Scopes: []string{"delegate", "inference"}, AllowedRoutes: []string{"shared"}},
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
	nonAgent := access.Authority{ProjectID: &project, WorkloadIssuerID: &issuer, SnapshotID: "snapshot"}
	missingIssuer := access.Authority{ProjectID: &project, AgentPrincipal: true, SnapshotID: "snapshot"}
	otherAgent, _ := delegationPrincipals(otherProject, issuer)
	nestedKey := access.Authority{ProjectID: &project, WorkloadIssuerID: &issuer, SnapshotID: "snapshot", Policy: key.Policy}
	unprivilegedKey := access.Authority{ProjectID: &project, SnapshotID: "snapshot", Policy: access.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"shared"}}}
	staleAgent := access.Authority{ProjectID: &project, AgentPrincipal: true, WorkloadIssuerID: &issuer, SnapshotID: "old", Policy: agent.Policy}
	restrictedAgent := access.Authority{ProjectID: &project, AgentPrincipal: true, WorkloadIssuerID: &issuer, SnapshotID: "snapshot", Policy: access.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"other"}}}
	restrictedKey := access.Authority{ProjectID: &project, SnapshotID: "snapshot", Policy: access.KeyPolicy{Scopes: []string{"delegate", "inference"}, AllowedRoutes: []string{"other"}}}
	revokedAt := time.Now()
	revokedKey := access.Authority{ProjectID: &project, SnapshotID: "snapshot", Policy: key.Policy, RevokedAt: &revokedAt}
	bounds := DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"shared"}}
	manager := &Manager{}
	for _, test := range []struct {
		name  string
		agent access.Authority
		key   access.Authority
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
			var problem *access.Problem
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
		agent access.Authority
		want  error
	}{
		{name: "missing agent", want: ErrInvalidKey},
		{name: "missing expiry", agent: agent, want: ErrInvalidKey},
		{name: "expired", agent: access.Authority{ProjectID: &project, AgentPrincipal: true, WorkloadIssuerID: &issuer, ExpiresAt: &expired}, want: ErrInvalidKey},
		{name: "revoked", agent: access.Authority{ProjectID: &project, AgentPrincipal: true, WorkloadIssuerID: &issuer, ExpiresAt: &active, RevokedAt: &now}, want: ErrInvalidKey},
		{name: "active requires current authority", agent: access.Authority{ProjectID: &project, AgentPrincipal: true, WorkloadIssuerID: &issuer, SnapshotID: "snapshot", ExpiresAt: &active}, want: ErrStaleAuthority},
	} {
		t.Run(test.name, func(t *testing.T) {
			grant, err := (&Manager{}).ExchangeMemberConsent(context.Background(), test.agent, "consent", DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"shared"}})
			if grant.Token != "" || !errors.Is(err, test.want) {
				t.Fatalf("consent exchange returned grant %+v and error %v, want %v", grant, err, test.want)
			}
		})
	}
}
