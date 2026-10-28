package routing

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/guardrails"
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
	resolved, ok := snapshot.MCPToolset(mcpRevision)
	if !ok || len(resolved.Tools) != 1 || len(resolved.Drifted) != 0 {
		t.Fatalf("pinned toolset did not resolve: ok=%v resolved=%+v", ok, resolved)
	}
	tool := resolved.Tools[0]
	if tool.Alias != agentcontract.ToolAlias(mcpServerID, "lookup") || tool.Name != "lookup" || tool.Endpoint != "https://tools.example.test/mcp" || tool.ServerID != mcpServerID || tool.ServerRevisionID != mcpServerRv || tool.CatalogDigest != digest || tool.CredentialID == nil || *tool.CredentialID != mcpCredID {
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
	if _, ok = snapshot.MCPToolset("0192cf87-d4ab-7f2e-a8b1-c2d3e4f50699"); ok {
		t.Fatal("unknown revision resolved as a toolset")
	}
}

func TestSnapshotReportsMCPToolsWhosePinDriftedFromCatalog(t *testing.T) {
	drift := map[string]func(*Snapshot){
		"catalog digest changed": func(snapshot *Snapshot) {
			snapshot.MCPServers[0].Digest = strings.Repeat("b", 64)
		},
		"server revision advanced": func(snapshot *Snapshot) {
			snapshot.MCPServers[0].RevisionID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50617"
		},
		"pinned tool left the catalog": func(snapshot *Snapshot) {
			snapshot.MCPServers[0].Tools[0].Name = "renamed"
		},
	}
	for name, mutate := range drift {
		t.Run(name, func(t *testing.T) {
			snapshot := mcpSnapshot(t, strings.Repeat("a", 64))
			mutate(&snapshot)
			if err := snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
			resolved, ok := snapshot.MCPToolset(mcpRevision)
			if !ok || len(resolved.Tools) != 0 {
				t.Fatalf("drifted pin stayed callable: ok=%v resolved=%+v", ok, resolved)
			}
			if len(resolved.Drifted) != 1 {
				t.Fatalf("drifted pin disappeared without a trace: %+v", resolved)
			}
			pin := resolved.Drifted[0]
			if pin.Alias != agentcontract.ToolAlias(mcpServerID, "lookup") || pin.Name != "lookup" || pin.ServerID != mcpServerID || pin.ServerRevisionID != mcpServerRv || pin.CatalogDigest != strings.Repeat("a", 64) {
				t.Fatalf("drifted pin lost its published identity: %+v", pin)
			}
		})
	}
}

func TestSnapshotMissingMCPServerIsNotReportedAsDrift(t *testing.T) {
	for name, mutate := range map[string]func(*Snapshot){
		"server gone": func(snapshot *Snapshot) {
			snapshot.MCPServers = nil
		},
		"server in another project": func(snapshot *Snapshot) {
			snapshot.MCPServers[0].ProjectID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50618"
		},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := mcpSnapshot(t, strings.Repeat("a", 64))
			mutate(&snapshot)
			if err := snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
			resolved, ok := snapshot.MCPToolset(mcpRevision)
			if !ok || len(resolved.Tools) != 0 || len(resolved.Drifted) != 0 {
				t.Fatalf("unavailable server was reported as callable or drifted: ok=%v resolved=%+v", ok, resolved)
			}
			if len(resolved.Missing) != 1 || resolved.Missing[0].Alias != agentcontract.ToolAlias(mcpServerID, "lookup") {
				t.Fatalf("unavailable server pin was not reported as missing: %+v", resolved)
			}
		})
	}
}

func TestSnapshotReportsUncompilableMCPSchemaAsInvalidNotDrifted(t *testing.T) {
	snapshot := mcpSnapshot(t, strings.Repeat("a", 64))
	snapshot.MCPServers[0].Tools[0].InputSchema = json.RawMessage(`{"type":"object","$ref":"https://schemas.example.test/tool"}`)
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	resolved, ok := snapshot.MCPToolset(mcpRevision)
	if !ok || len(resolved.Tools) != 0 || len(resolved.Drifted) != 0 {
		t.Fatalf("uncompilable schema stayed callable or was reported as drift: ok=%v resolved=%+v", ok, resolved)
	}
	if len(resolved.Invalid) != 1 || resolved.Invalid[0].Alias != agentcontract.ToolAlias(mcpServerID, "lookup") {
		t.Fatalf("uncompilable schema was not reported as invalid: %+v", resolved)
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

func TestSnapshotResolvesPinnedToolPricesAndLimits(t *testing.T) {
	digest := strings.Repeat("a", 64)
	snapshot := mcpSnapshot(t, digest)
	serverPrice := "0.10"
	snapshot.MCPServers[0].UnitPrice = &serverPrice
	snapshot.MCPServers[0].ToolPrices = map[string]string{"lookup": "0.25"}
	snapshot.MCPServers[0].CallsPerMinute = 120
	snapshot.MCPServers[0].MaxConcurrency = 4
	snapshot.MCPServers[0].PerKeyCallsPerMinute = 30
	snapshot.MCPServers[0].PerKeyMaxConcurrency = 2
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	resolved, ok := snapshot.MCPToolset(mcpRevision)
	if !ok || len(resolved.Tools) != 1 {
		t.Fatalf("priced toolset did not resolve: ok=%v resolved=%+v", ok, resolved)
	}
	tool := resolved.Tools[0]
	if tool.UnitPrice == nil || *tool.UnitPrice != "0.25" {
		t.Fatalf("per-tool price did not override the server price: %+v", tool.UnitPrice)
	}
	if tool.CallsPerMinute != 120 || tool.MaxConcurrency != 4 || tool.PerKeyCallsPerMinute != 30 || tool.PerKeyMaxConcurrency != 2 {
		t.Fatalf("resolved tool lost its server limits: %+v", tool)
	}

	snapshot = mcpSnapshot(t, digest)
	snapshot.MCPServers[0].UnitPrice = &serverPrice
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	resolved, _ = snapshot.MCPToolset(mcpRevision)
	tool = resolved.Tools[0]
	if tool.UnitPrice == nil || *tool.UnitPrice != serverPrice {
		t.Fatalf("server price did not reach the tool: %+v", tool.UnitPrice)
	}
	if tool.CallsPerMinute != DefaultToolCallsPerMinute || tool.MaxConcurrency != DefaultToolMaxConcurrency ||
		tool.PerKeyCallsPerMinute != DefaultToolCallsPerMinute || tool.PerKeyMaxConcurrency != DefaultToolMaxConcurrency {
		t.Fatalf("default tool limits did not apply: %+v", tool)
	}

	snapshot = mcpSnapshot(t, digest)
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	resolved, _ = snapshot.MCPToolset(mcpRevision)
	if resolved.Tools[0].UnitPrice != nil {
		t.Fatalf("missing price became a price: %+v", resolved.Tools[0].UnitPrice)
	}
	free := "0"
	snapshot = mcpSnapshot(t, digest)
	snapshot.MCPServers[0].UnitPrice = &free
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	resolved, _ = snapshot.MCPToolset(mcpRevision)
	if resolved.Tools[0].UnitPrice == nil || *resolved.Tools[0].UnitPrice != "0" {
		t.Fatalf("explicit zero price did not stay known free: %+v", resolved.Tools[0].UnitPrice)
	}
}

func TestSnapshotRefusesMalformedToolPricesAndLimits(t *testing.T) {
	digest := strings.Repeat("a", 64)
	negative := "-1"
	for name, mutate := range map[string]func(*Snapshot){
		"negative server price": func(s *Snapshot) { s.MCPServers[0].UnitPrice = &negative },
		"malformed tool price":  func(s *Snapshot) { s.MCPServers[0].ToolPrices = map[string]string{"lookup": "1e3"} },
		"price for unknown tool": func(s *Snapshot) {
			s.MCPServers[0].ToolPrices = map[string]string{"absent": "1"}
		},
		"rate above installation ceiling": func(s *Snapshot) { s.MCPServers[0].CallsPerMinute = MaxToolCallsPerMinute + 1 },
		"negative concurrency":            func(s *Snapshot) { s.MCPServers[0].MaxConcurrency = -1 },
		"per-key concurrency above ceiling": func(s *Snapshot) {
			s.MCPServers[0].PerKeyMaxConcurrency = MaxToolConcurrency + 1
		},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := mcpSnapshot(t, digest)
			mutate(&snapshot)
			if err := snapshot.Compile(); err == nil {
				t.Fatalf("snapshot accepted %s", name)
			}
		})
	}
	snapshot := mcpSnapshot(t, digest)
	snapshot.MCPServers[0].ToolPrices = map[string]string{"lookup": "0.5"}
	clone := snapshot.Clone()
	clone.MCPServers[0].ToolPrices["lookup"] = "9"
	if snapshot.MCPServers[0].ToolPrices["lookup"] != "0.5" {
		t.Fatal("snapshot clone shares mutable tool prices")
	}
}

func TestSnapshotCompilesToolGuardrailEnginesPerToolsetAndKey(t *testing.T) {
	snapshot := mcpSnapshot(t, strings.Repeat("a", 64))
	definition := func(id string) guardrails.Revision {
		return guardrails.Revision{ID: id, GuardrailID: id, ProjectID: mcpProject, Type: "builtin.keywords", Configuration: json.RawMessage(`{"words":["private"]}`), TimeoutMS: 100, OnError: guardrails.FailClosed}
	}
	policy := func(id, definitionID, phase string) guardrails.PolicyRevision {
		return guardrails.PolicyRevision{ID: id, PolicyID: id, ProjectID: mcpProject, Entries: []guardrails.Entry{{GuardrailID: definitionID, RevisionID: definitionID, Phase: phase, Action: guardrails.Block, Mandatory: true}}}
	}
	const (
		toolDef    = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50621"
		toolPol    = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50622"
		projectDef = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50623"
		projectPol = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50624"
		keyDef     = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50625"
		keyPol     = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50626"
		keyID      = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50627"
	)
	snapshot.Guardrails = &guardrails.Registry{
		Definitions: map[string]guardrails.Revision{toolDef: definition(toolDef), projectDef: definition(projectDef), keyDef: definition(keyDef)},
		Policies: map[string]guardrails.PolicyRevision{
			toolPol:    policy(toolPol, toolDef, guardrails.Input),
			projectPol: policy(projectPol, projectDef, guardrails.Output),
			keyPol:     policy(keyPol, keyDef, guardrails.OutputStream),
		},
		Attachments: []guardrails.Attachment{
			{ID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50628", Scope: "toolset", ScopeID: mcpToolset, PolicyRevisionID: toolPol},
			{ID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50629", Scope: "project", ScopeID: mcpProject, PolicyRevisionID: projectPol},
			{ID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50630", Scope: "api-key", ScopeID: keyID, PolicyRevisionID: keyPol},
		},
		ProjectOrganizations: map[string]string{},
	}
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	engine := snapshot.ToolGuardrailEngine(mcpToolset, "")
	if engine == nil || !engine.HasPhase(guardrails.Input) || !engine.HasPhase(guardrails.Output) {
		t.Fatalf("tool engine lost its toolset or project policies: %v", engine)
	}
	if engine.HasPhase(guardrails.OutputStream) {
		t.Fatal("a key policy reached calls from other keys")
	}
	keyEngine := snapshot.ToolGuardrailEngine(mcpToolset, keyID)
	if keyEngine == nil || !keyEngine.HasPhase(guardrails.Input) || !keyEngine.HasPhase(guardrails.OutputStream) {
		t.Fatalf("key tool engine lost its inherited policies: %v", keyEngine)
	}
	if snapshot.ToolGuardrailEngine("0192cf87-d4ab-7f2e-a8b1-c2d3e4f50699", "") != nil {
		t.Fatal("an unknown toolset resolved a guardrail engine")
	}
	plain := mcpSnapshot(t, strings.Repeat("a", 64))
	if err := plain.Compile(); err != nil {
		t.Fatal(err)
	}
	if plain.ToolGuardrailEngine(mcpToolset, "") != nil {
		t.Fatal("a snapshot without guardrails compiled a tool engine")
	}
}

func TestSnapshotRefusesMoreThan128MCPServers(t *testing.T) {
	digest := strings.Repeat("a", 64)
	servers := func(count int) []MCPServer {
		out := make([]MCPServer, count)
		for i := range out {
			out[i] = MCPServer{
				ID:         fmt.Sprintf("0192cf87-d4ab-7f2e-a8b1-c2d3e4f5%04x", i),
				ProjectID:  mcpProject,
				RevisionID: fmt.Sprintf("0192cf87-d4ab-7f2e-a8b1-c2d3e4f6%04x", i),
				Endpoint:   "https://tools.example.test/mcp", Protocol: "2025-11-25", Digest: digest,
			}
		}
		return out
	}
	snapshot := fidelitySnapshot(t)
	snapshot.MCPServers = servers(128)
	if err := snapshot.Compile(); err != nil {
		t.Fatalf("128 MCP servers refused: %v", err)
	}
	snapshot = fidelitySnapshot(t)
	snapshot.MCPServers = servers(129)
	if err := snapshot.Compile(); err == nil {
		t.Fatal("snapshot accepted more than 128 MCP servers")
	}
}
