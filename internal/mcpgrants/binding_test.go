package mcpgrants

import (
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
)

func testOAuth(scopes ...string) *routing.MCPOAuth {
	return &routing.MCPOAuth{
		ClientID:              "client",
		Audience:              "api://tools",
		Scopes:                scopes,
		AuthorizationEndpoint: "https://issuer.example.com/authorize",
		TokenEndpoint:         "https://issuer.example.com/token",
	}
}

func testAuthority() access.Authority {
	project := "project-1"
	return access.Authority{ID: "key-1", ProjectID: &project}
}

func testTool(scopes ...string) routing.MCPToolsetTool {
	return routing.MCPToolsetTool{ServerID: "server-1", Endpoint: "https://tools.example.com/mcp", OAuth: testOAuth(scopes...)}
}

func TestBindRequiresProjectAndOAuth(t *testing.T) {
	noProject := testAuthority()
	noProject.ProjectID = nil
	if _, err := Bind(noProject, testTool("read")); !errors.Is(err, ErrRequired) {
		t.Errorf("Bind without project err = %v, want ErrRequired", err)
	}
	tool := testTool("read")
	tool.OAuth = nil
	if _, err := Bind(testAuthority(), tool); !errors.Is(err, ErrRequired) {
		t.Errorf("Bind without OAuth err = %v, want ErrRequired", err)
	}
	if _, err := Bind(access.Authority{ProjectID: testAuthority().ProjectID}, testTool("read")); !errors.Is(err, ErrRequired) {
		t.Errorf("Bind without any identity err = %v, want ErrRequired", err)
	}
}

func TestBindCanonicalizesScopes(t *testing.T) {
	bound, err := Bind(testAuthority(), testTool("write", "read", "read"))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	want := []string{"read", "write"}
	if len(bound.OAuth.Scopes) != len(want) {
		t.Fatalf("scopes = %v, want %v", bound.OAuth.Scopes, want)
	}
	for i := range want {
		if bound.OAuth.Scopes[i] != want[i] {
			t.Fatalf("scopes = %v, want %v", bound.OAuth.Scopes, want)
		}
	}
	if bound.ServerID != "server-1" || bound.ProjectID != "project-1" || bound.Identity != (Identity{Kind: KeyIdentity, ID: "key-1"}) {
		t.Fatalf("binding = %+v", bound)
	}
}

func TestBindDigestIgnoresScopeOrderButTracksEndpoint(t *testing.T) {
	first, err := Bind(testAuthority(), testTool("write", "read"))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	second, err := Bind(testAuthority(), testTool("read", "write"))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if first.Digest() != second.Digest() {
		t.Error("digests differ for bindings that only reorder scopes")
	}
	tool := testTool("read", "write")
	tool.Endpoint = "https://other.example.com/mcp"
	third, err := Bind(testAuthority(), tool)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if first.Digest() == third.Digest() {
		t.Error("digests match for bindings to different endpoints")
	}
}

func TestBindPrefersEndUserIdentity(t *testing.T) {
	authority := testAuthority()
	issuer := "issuer-1"
	authority.WorkloadIssuerID = &issuer
	authority.EndUserDigest = "digest-1"
	bound, err := Bind(authority, testTool("read"))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	want := Identity{Kind: EndUserIdentity, ID: "digest-1", WorkloadIssuerID: "issuer-1"}
	if bound.Identity != want {
		t.Fatalf("identity = %+v, want %+v", bound.Identity, want)
	}
}

func TestBindingSealPurposes(t *testing.T) {
	grant := Binding{}
	if grant.accessPurpose() != secrets.MCPGrantAccess || grant.refreshPurpose() != secrets.MCPGrantRefresh {
		t.Error("grant binding does not use the MCP grant seal purposes")
	}
	agent := Binding{AgentID: "agent-1"}
	if agent.accessPurpose() != secrets.AgentCredential || agent.refreshPurpose() != secrets.AgentCredential {
		t.Error("agent binding does not use the agent credential seal purpose")
	}
}

func TestBindAgentEntraCredentialPairing(t *testing.T) {
	credential := "credential-1"
	plain := agentcontract.Definition{Endpoint: "https://agent.example.com", OAuth: testOAuth("read"), CredentialID: &credential}
	if _, err := BindAgent(testAuthority(), "pub-1", plain); !errors.Is(err, ErrRequired) {
		t.Errorf("BindAgent with a credential but no Entra config err = %v, want ErrRequired", err)
	}
	plain.CredentialID = nil
	bound, err := BindAgent(testAuthority(), "pub-1", plain)
	if err != nil {
		t.Fatalf("BindAgent: %v", err)
	}
	if bound.AgentID != "pub-1" || bound.EntraCredentialID != "" || bound.ServerID != "" {
		t.Fatalf("agent binding = %+v", bound)
	}
	if _, err := BindAgent(testAuthority(), "", plain); !errors.Is(err, ErrRequired) {
		t.Errorf("BindAgent without publication err = %v, want ErrRequired", err)
	}
}
