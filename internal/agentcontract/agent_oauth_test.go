package agentcontract_test

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/grants/config"
)

func TestAgentOAuthRequiresSupportedExclusiveCredential(t *testing.T) {
	const id = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	browser := &config.OAuth{ClientID: "client", Audience: "audience", Scopes: []string{"read"}, AuthorizationEndpoint: "https://auth.example/authorize", TokenEndpoint: "https://auth.example/token"}
	for _, test := range []struct {
		name       string
		oauth      *config.OAuth
		credential *string
		allowed    bool
	}{
		{name: "browser", oauth: browser, allowed: true},
		{name: "no credentials", allowed: true},
		{name: "static credential", credential: new(id), allowed: true},
		{name: "static and oauth", oauth: browser, credential: new(id)},
		{name: "incomplete oauth", oauth: &config.OAuth{ClientID: "client"}},
		{name: "entra on behalf of", oauth: &config.OAuth{ClientID: "client", Audience: "api", Scopes: []string{"api/read"}, TokenEndpoint: "https://auth.example/token", EntraOBO: &config.EntraOBO{Issuer: "https://auth.example"}}, allowed: true},
		{name: "invalid entra issuer", oauth: &config.OAuth{ClientID: "client", Audience: "api", Scopes: []string{"api/read"}, TokenEndpoint: "https://auth.example/token", EntraOBO: &config.EntraOBO{Issuer: "https://auth.example/?query=one"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			draft := agentcontract.Draft{ProjectID: id, Name: "agent", Agent: &agentcontract.Definition{Protocol: "a2a-1.0.0", Endpoint: "https://agent.example/rpc", CardDigest: strings.Repeat("a", 64), PricingRevisionID: id, MaxCost: "1", OAuth: test.oauth, CredentialID: test.credential}}
			if err := draft.Validate("agent"); (err == nil) != test.allowed {
				t.Fatalf("OAuth credential allowed = %t, want %t: %v", err == nil, test.allowed, err)
			}
		})
	}
}
