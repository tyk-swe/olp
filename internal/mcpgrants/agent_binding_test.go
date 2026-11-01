package mcpgrants_test

import (
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/grants/config"
	"github.com/tyk-swe/olp/internal/mcpgrants"
)

func TestAgentGrantBindingRespectsResourceAndCaller(t *testing.T) {
	project, issuer := "project", "issuer"
	oauth := &config.OAuth{ClientID: "client", Audience: "audience", Scopes: []string{"read", "write"}, AuthorizationEndpoint: "https://auth.example/authorize", TokenEndpoint: "https://auth.example/token"}
	agent := agentcontract.Definition{Endpoint: "https://agent.example/rpc", OAuth: oauth}
	bind := func(authority access.Authority, publication string, definition agentcontract.Definition) mcpgrants.Binding {
		t.Helper()
		result, err := mcpgrants.BindAgent(authority, publication, definition)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	authority := access.Authority{ID: "workload-key", ProjectID: &project, WorkloadIssuerID: &issuer, EndUserDigest: "alice"}
	original := bind(authority, "agent", agent)
	otherEndpoint := agentcontract.Definition{Endpoint: "https://other.example/rpc", OAuth: oauth}
	otherOAuth := agentcontract.Definition{Endpoint: agent.Endpoint, OAuth: &config.OAuth{ClientID: "other-client", Audience: oauth.Audience, Scopes: oauth.Scopes, AuthorizationEndpoint: oauth.AuthorizationEndpoint, TokenEndpoint: oauth.TokenEndpoint}}
	for name, other := range map[string]mcpgrants.Binding{
		"publication": bind(authority, "other-agent", agent),
		"endpoint":    bind(authority, "agent", otherEndpoint),
		"oauth":       bind(authority, "agent", otherOAuth),
		"end user":    bind(access.Authority{ID: authority.ID, ProjectID: &project, WorkloadIssuerID: &issuer, EndUserDigest: "bob"}, "agent", agent),
		"api key":     bind(access.Authority{ID: authority.ID, ProjectID: &project}, "agent", agent),
	} {
		t.Run(name, func(t *testing.T) {
			if original.Digest() == other.Digest() {
				t.Fatal("grant binding crossed an authority boundary")
			}
		})
	}
	reordered := agentcontract.Definition{Endpoint: agent.Endpoint, OAuth: &config.OAuth{ClientID: oauth.ClientID, Audience: oauth.Audience, Scopes: []string{"write", "read", "read"}, AuthorizationEndpoint: oauth.AuthorizationEndpoint, TokenEndpoint: oauth.TokenEndpoint}}
	if bind(authority, "agent", reordered).Digest() != original.Digest() {
		t.Fatal("equivalent scope sets changed the binding")
	}
	if original.AgentID != "agent" || original.ServerID != "" {
		t.Fatal("agent grant was not bound to its agent")
	}
	entra := &config.OAuth{ClientID: "client", Audience: "audience", Scopes: []string{"audience/read"}, TokenEndpoint: "https://auth.example/token", EntraOBO: &config.EntraOBO{Issuer: "https://identity.example"}}
	oldSecret, newSecret := "old-secret-id", "new-secret-id"
	oldAgent := agentcontract.Definition{Endpoint: agent.Endpoint, OAuth: entra, CredentialID: &oldSecret}
	newAgent := agentcontract.Definition{Endpoint: agent.Endpoint, OAuth: entra, CredentialID: &newSecret}
	if bind(authority, "agent", oldAgent).Digest() == bind(authority, "agent", newAgent).Digest() {
		t.Fatal("replacing an Entra client secret reused its original grant binding")
	}
}
