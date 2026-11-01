package mcpgrants

import (
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestPersonalGrantBindingSeparatesEveryAuthorityBoundary(t *testing.T) {
	project, otherProject, issuer := "project", "other-project", "issuer"
	config := func(client, audience string, scopes []string) *routing.MCPOAuth {
		return &routing.MCPOAuth{ClientID: client, Audience: audience, Scopes: scopes, AuthorizationEndpoint: "https://identity.example.test/authorize", TokenEndpoint: "https://identity.example.test/token"}
	}
	standard := config("client", "audience", []string{"read", "write"})
	bind := func(authority access.Authority, server string, oauth *routing.MCPOAuth) Binding {
		t.Helper()
		value, err := Bind(authority, routing.MCPToolsetTool{ServerID: server, Endpoint: "https://tools.example.test/mcp", OAuth: oauth})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	authority := access.Authority{ID: "key", ProjectID: &project, WorkloadIssuerID: &issuer, EndUserDigest: "individual"}
	current := bind(authority, "server", standard)
	for name, different := range map[string]Binding{
		"server":           bind(authority, "other-server", standard),
		"client":           bind(authority, "server", config("other-client", "audience", standard.Scopes)),
		"audience":         bind(authority, "server", config("client", "other-audience", standard.Scopes)),
		"scope":            bind(authority, "server", config("client", "audience", []string{"read"})),
		"project":          bind(access.Authority{ID: "key", ProjectID: &otherProject, WorkloadIssuerID: &issuer, EndUserDigest: "individual"}, "server", standard),
		"individual":       bind(access.Authority{ID: "key", ProjectID: &project, WorkloadIssuerID: &issuer, EndUserDigest: "other-individual"}, "server", standard),
		"unverified label": bind(access.Authority{ID: "key", ProjectID: &project, EndUserDigest: "individual"}, "server", standard),
	} {
		t.Run(name, func(t *testing.T) {
			if different.Digest() == current.Digest() {
				t.Fatal("a grant could cross its authority boundary")
			}
		})
	}
	reordered := bind(authority, "server", config("client", "audience", []string{"write", "read", "read"}))
	if reordered.Digest() != current.Digest() {
		t.Fatal("equivalent scope sets changed the grant binding")
	}
	otherLabel := bind(access.Authority{ID: "key", ProjectID: &project, EndUserDigest: "other-label"}, "server", standard)
	plainKey := bind(access.Authority{ID: "key", ProjectID: &project}, "server", standard)
	if otherLabel.Digest() != plainKey.Digest() {
		t.Fatal("an accounting label selected a different key grant")
	}
}
