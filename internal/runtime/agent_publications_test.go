package runtime

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

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
