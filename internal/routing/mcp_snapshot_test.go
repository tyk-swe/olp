package routing

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/agentcontract"
)

const (
	mcpProject  = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50611"
	mcpServerID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50612"
	mcpServerRv = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50613"
	mcpToolset  = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50614"
	mcpRevision = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50615"
	mcpCredID   = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50616"
)

var mcpSchema = json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)

func mcpSnapshot(t *testing.T, digest string) Snapshot {
	t.Helper()
	snapshot := fidelitySnapshot(t)
	cred := mcpCredID
	snapshot.MCPServers = []MCPServer{{
		ID: mcpServerID, ProjectID: mcpProject, RevisionID: mcpServerRv,
		Endpoint: "https://tools.example.test/mcp", Protocol: "2025-11-25",
		Digest: digest, CredentialID: &cred,
		Tools: []MCPTool{{Name: "lookup", Description: "Look up", InputSchema: mcpSchema}},
	}}
	tool := agentcontract.Tool{ServerID: mcpServerID, RevisionID: mcpServerRv, CatalogDigest: digest, Name: "lookup", Alias: agentcontract.ToolAlias(mcpServerID, "lookup")}
	draft := agentcontract.Draft{ProjectID: mcpProject, Name: "tools", Toolset: &agentcontract.Toolset{Tools: []agentcontract.Tool{tool}}}
	snapshot.AgentPublications = []agentcontract.Revision{{ID: mcpRevision, PublicationID: mcpToolset, ProjectID: mcpProject, Kind: "toolset", Digest: agentcontract.DraftDigest(draft), Definition: draft, Current: true}}
	return snapshot
}

func TestSnapshotResolvesPinnedMCPToolAndValidatesArguments(t *testing.T) {
	digest := strings.Repeat("a", 64)
	snapshot := mcpSnapshot(t, digest)
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	tools, ok := snapshot.MCPTools(mcpRevision)
	if !ok || len(tools) != 1 {
		t.Fatalf("pinned toolset did not resolve: ok=%v tools=%+v", ok, tools)
	}
	tool := tools[0]
	if tool.Alias != agentcontract.ToolAlias(mcpServerID, "lookup") || tool.Name != "lookup" || tool.Endpoint != "https://tools.example.test/mcp" || tool.ServerID != mcpServerID || tool.ServerRevisionID != mcpServerRv || tool.CredentialID == nil || *tool.CredentialID != mcpCredID {
		t.Fatalf("resolved tool lost its pin: %+v", tool)
	}
	if err := tool.ValidateArguments(json.RawMessage(`{"query":"x"}`)); err != nil {
		t.Fatal("valid arguments refused", err)
	}
	for _, bad := range []string{`{}`, `{"query":1}`, `{"query":"x","extra":1}`, `[]`, `not json`} {
		if err := tool.ValidateArguments(json.RawMessage(bad)); err == nil {
			t.Fatalf("invalid arguments accepted: %s", bad)
		}
	}
	if _, ok = snapshot.MCPTools("0192cf87-d4ab-7f2e-a8b1-c2d3e4f50699"); ok {
		t.Fatal("unknown revision resolved as a toolset")
	}
}

func TestSnapshotExcludesMCPToolsWhosePinDriftedFromCatalog(t *testing.T) {
	snapshot := mcpSnapshot(t, strings.Repeat("a", 64))
	snapshot.MCPServers[0].Digest = strings.Repeat("b", 64)
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	tools, ok := snapshot.MCPTools(mcpRevision)
	if !ok || len(tools) != 0 {
		t.Fatalf("drifted pin stayed callable: ok=%v tools=%+v", ok, tools)
	}
}

func TestSnapshotMCPCloneAndDigest(t *testing.T) {
	snapshot := mcpSnapshot(t, strings.Repeat("a", 64))
	want, err := snapshot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	clone := snapshot.Clone()
	clone.MCPServers[0].Tools[0].InputSchema[0] = '['
	*clone.MCPServers[0].CredentialID = mcpRevision
	if snapshot.MCPServers[0].Tools[0].InputSchema[0] != '{' || *snapshot.MCPServers[0].CredentialID != mcpCredID {
		t.Fatal("snapshot clone shares mutable MCP servers")
	}
	clone.MCPServers[0].Tools[0].InputSchema[0] = '{'
	got, err := clone.Digest()
	if err != nil || got == want {
		t.Fatal("MCP server mutation did not change serving digest", err)
	}
}
