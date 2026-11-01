package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
)

type discoveryCatalog map[string][]WorkflowTool

func (catalog discoveryCatalog) Resolve(_ context.Context, _ access.Authority, _ *runtime.Release, toolsets []string) ([]WorkflowTool, error) {
	if slices.ContainsFunc(toolsets, func(id string) bool { _, ok := catalog[id]; return !ok }) {
		return nil, errors.New("toolset unavailable")
	}
	return slices.Collect(func(yield func(WorkflowTool) bool) {
		for _, id := range toolsets {
			for _, tool := range catalog[id] {
				if !yield(tool) {
					return
				}
			}
		}
	}), nil
}

func TestWorkflowDiscoveryRespectsAuthorityAndReportsToolMetadata(t *testing.T) {
	project := testProject
	price := "0.025"
	limit := int64(4)
	callerLimit := int64(2)
	authority := access.Authority{ProjectID: &project, Policy: access.KeyPolicy{Scopes: []string{"inference", "tools"}, AllowedRoutes: []string{"workflow"}, AllowedToolsets: []string{testToolset}, RunLimits: &limits.WorkflowBounds{MaxIterations: &limit}}}
	provider := routing.Provider{ID: "fixture", Kind: "openai", VendorID: "openai", Enabled: true, Capabilities: []routing.Capability{{Model: "fixture-model", Operation: "generation", Surface: "openai", Mode: "unary", ToolCalls: true}}}
	route := routing.Route{Slug: "workflow", ProjectID: &project, Operations: []string{"generation"}, Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed}, Targets: []routing.Target{{ProviderID: "fixture", ProviderModel: "fixture-model"}}, Behavior: routing.Behavior{ToolExecution: "gateway", Toolsets: []string{testToolset, testOtherTS}}}
	release := &runtime.Release{Snapshot: &routing.Snapshot{Providers: map[string]routing.Provider{"fixture": provider}}}
	catalog := discoveryCatalog{
		testToolset: {{ToolsetID: testToolset, RevisionID: testRevision, Definition: routing.MCPToolsetTool{Name: "search", Alias: "search_alias", UnitPrice: &price, Description: "private description", InputSchema: json.RawMessage(`{"secret":"private schema"}`)}}},
		testOtherTS: {{ToolsetID: testOtherTS, RevisionID: testOtherRv, Definition: routing.MCPToolsetTool{Name: "restricted", Alias: "restricted_alias", Classification: "read_only"}}},
	}
	capability := DescribeWorkflow(t.Context(), catalog, authority, release, route, route, "openai", &limits.WorkflowBounds{MaxIterations: &callerLimit}, true, time.Now())
	if capability == nil || !capability.Enabled || capability.DefaultToolsetsAllowed || !slices.Equal(capability.CertifiedModes, []string{"unary"}) {
		t.Fatalf("unexpected accessible capability: %+v", capability)
	}
	if capability.Limits.MaxIterations == nil || *capability.Limits.MaxIterations != callerLimit {
		t.Fatalf("caller limit not reflected: %+v", capability.Limits)
	}
	if len(capability.Tools) != 1 || capability.Tools[0].ToolsetID != testToolset || capability.Tools[0].RevisionID != testRevision || capability.Tools[0].Alias != "search_alias" || capability.Tools[0].Name != "search" || capability.Tools[0].Classification != "approval_required" || capability.Tools[0].UnitPrice == nil || *capability.Tools[0].UnitPrice != price {
		t.Fatalf("unexpected authorized tool metadata: %+v", capability.Tools)
	}
	encoded, err := json.Marshal(capability)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc([]string{"restricted_alias", "private description", "private schema"}, func(secret string) bool { return strings.Contains(string(encoded), secret) }) {
		t.Fatalf("capability disclosed restricted tool details: %s", encoded)
	}
	otherProject := testOtherTS
	otherAuthority := access.Authority{ProjectID: &otherProject, Policy: authority.Policy}
	if got := DescribeWorkflow(t.Context(), catalog, otherAuthority, release, route, route, "openai", nil, true, time.Now()); got != nil {
		t.Fatalf("cross-project route was disclosed: %+v", got)
	}
	withoutTools := access.Authority{ProjectID: &project, Policy: access.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"workflow"}, AllowedToolsets: []string{testToolset}}}
	withoutToolsCapability := DescribeWorkflow(t.Context(), catalog, withoutTools, release, route, route, "openai", nil, true, time.Now())
	if withoutToolsCapability == nil || withoutToolsCapability.DefaultToolsetsAllowed || len(withoutToolsCapability.Tools) != 0 {
		t.Fatalf("tool details disclosed without tools scope: %+v", withoutToolsCapability)
	}
	strict := routing.Route{Slug: "workflow", ProjectID: &project, Fidelity: routing.RouteFidelity{Mode: routing.FidelityStrict}, Behavior: route.Behavior}
	if got := DescribeWorkflow(t.Context(), catalog, authority, release, strict, strict, "openai", nil, true, time.Now()); got != nil {
		t.Fatalf("strict route advertised gateway execution: %+v", got)
	}
}

func TestWorkflowToolsetSelectionCanOnlyNarrowTheRoute(t *testing.T) {
	allowed := []string{testToolset, testOtherTS}
	for _, test := range []struct {
		name string
		raw  json.RawMessage
		want []string
		err  error
	}{
		{name: "omitted uses route toolsets", want: allowed},
		{name: "empty selects no tools", raw: json.RawMessage(`[]`), want: []string{}},
		{name: "one permitted toolset", raw: json.RawMessage(`["` + testOtherTS + `"]`), want: []string{testOtherTS}},
		{name: "null is not a selection", raw: json.RawMessage(`null`), err: ErrWorkflowToolsetsInvalid},
		{name: "object is not a selection", raw: json.RawMessage(`{}`), err: ErrWorkflowToolsetsInvalid},
		{name: "unknown toolset", raw: json.RawMessage(`["other"]`), err: ErrWorkflowToolsetsDenied},
		{name: "duplicate toolset", raw: json.RawMessage(`["` + testToolset + `","` + testToolset + `"]`), err: ErrWorkflowToolsetsDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := SelectWorkflowToolsets(allowed, test.raw)
			if !errors.Is(err, test.err) || !slices.Equal(got, test.want) {
				t.Fatalf("selection = %v, error = %v; want %v, %v", got, err, test.want, test.err)
			}
		})
	}
}

func TestWorkflowRequestRefusesUnsupportedChoices(t *testing.T) {
	for _, test := range []struct {
		name   string
		family wire.Format
		body   string
		want   string
	}{
		{name: "chat default", family: wire.Chat, body: `{"model":"route","messages":[{"role":"user","content":"hi"}]}`},
		{name: "chat one choice", family: wire.Chat, body: `{"model":"route","messages":[{"role":"user","content":"hi"}],"n":1}`},
		{name: "chat multiple choices", family: wire.Chat, body: `{"model":"route","messages":[{"role":"user","content":"hi"}],"n":2}`, want: "tool_execution_choices_unsupported"},
		{name: "gemini default", family: wire.Gemini, body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
		{name: "gemini multiple candidates", family: wire.Gemini, body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"candidateCount":2}}`, want: "tool_execution_choices_unsupported"},
		{name: "gemini snake case multiple candidates", family: wire.Gemini, body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generation_config":{"candidate_count":2}}`, want: "tool_execution_choices_unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := protocols.Parse(test.family, []byte(test.body), "route")
			if err != nil {
				t.Fatal(err)
			}
			if got := WorkflowRequestRefusal(request); got != test.want {
				t.Fatalf("refusal = %q, want %q", got, test.want)
			}
		})
	}
	if got := WorkflowRequestRefusal(&wire.Request{Family: wire.Embeddings}); got != "tool_execution_unsupported" {
		t.Fatalf("unsupported surface refusal = %q", got)
	}
}

func TestWorkflowDiscoveryFollowsOnlyReachableCertifiedRoutes(t *testing.T) {
	project := testProject
	streaming := true
	provider := func(id string, unary, stream bool, metadata string) routing.Provider {
		return routing.Provider{ID: id, Kind: "openai", VendorID: "openai", Enabled: true, Models: map[string]json.RawMessage{"fixture-model": json.RawMessage(metadata)}, Capabilities: []routing.Capability{
			{Model: "fixture-model", Operation: "generation", Surface: "openai", Mode: "unary", ToolCalls: unary},
			{Model: "fixture-model", Operation: "generation", Surface: "openai", Mode: "streaming", ToolCalls: stream},
		}}
	}
	providers := map[string]routing.Provider{
		"primary":   provider("primary", false, false, `{"context_length":128}`),
		"unbounded": provider("unbounded", false, false, `{}`),
		"unary":     provider("unary", true, false, `{}`),
		"both":      provider("both", true, true, `{}`),
	}
	target := func(id string, tags ...string) routing.Target {
		return routing.Target{ProviderID: id, ProviderModel: "fixture-model", Tags: tags}
	}
	route := func(slug string, targets []routing.Target, behavior routing.Behavior) routing.Route {
		return routing.Route{Slug: slug, ProjectID: &project, Operations: []string{"generation"}, Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed}, Targets: targets, Behavior: behavior}
	}
	backup := route("backup", []routing.Target{target("both", "selected")}, routing.Behavior{})
	allowed := access.Authority{ProjectID: &project, Policy: access.KeyPolicy{Scopes: []string{"inference", "tools"}, AllowedRoutes: []string{"workflow", "backup"}, AllowedToolsets: []string{testToolset}}}
	forbiddenBackup := access.Authority{ProjectID: &project, Policy: access.KeyPolicy{Scopes: []string{"inference", "tools"}, AllowedRoutes: []string{"workflow"}, AllowedToolsets: []string{testToolset}}}
	catalog := discoveryCatalog{testToolset: {{ToolsetID: testToolset, RevisionID: testRevision, Definition: routing.MCPToolsetTool{Alias: "lookup", Classification: "read_only"}}}}
	for _, test := range []struct {
		name      string
		targets   []routing.Target
		selectors []routing.Selector
		fallbacks []routing.Fallback
		authority access.Authority
		surface   string
		available bool
		wantModes []string
		enabled   bool
	}{
		{name: "direct certified", targets: []routing.Target{target("unary")}, authority: allowed, surface: "openai", available: true, wantModes: []string{"unary"}, enabled: true},
		{name: "disabled by workflow availability", targets: []routing.Target{target("unary")}, authority: allowed, surface: "openai", wantModes: []string{"unary"}},
		{name: "uncertified target", targets: []routing.Target{target("primary")}, authority: allowed, surface: "openai", available: true, wantModes: []string{}},
		{name: "shadow target is not eligible", targets: []routing.Target{{ProviderID: "both", ProviderModel: "fixture-model", Shadow: &routing.Shadow{}}}, authority: allowed, surface: "openai", available: true, wantModes: []string{}},
		{name: "different surface", targets: []routing.Target{target("both")}, authority: allowed, surface: "anthropic", available: true, wantModes: []string{}},
		{name: "selected route", targets: []routing.Target{target("primary")}, selectors: []routing.Selector{{Route: "backup"}}, authority: allowed, surface: "openai", available: true, wantModes: []string{"unary", "streaming"}, enabled: true},
		{name: "forbidden selected route", targets: []routing.Target{target("primary")}, selectors: []routing.Selector{{Route: "backup"}}, authority: forbiddenBackup, surface: "openai", available: true, wantModes: []string{}},
		{name: "selected tags", targets: []routing.Target{target("primary"), target("both", "selected")}, selectors: []routing.Selector{{Tags: []string{"selected"}}}, authority: allowed, surface: "openai", available: true, wantModes: []string{"unary", "streaming"}, enabled: true},
		{name: "streaming selector", targets: []routing.Target{target("unary")}, selectors: []routing.Selector{{Route: "backup", When: routing.Predicate{Streaming: &streaming}}}, authority: allowed, surface: "openai", available: true, wantModes: []string{"unary", "streaming"}, enabled: true},
		{name: "context fallback", targets: []routing.Target{target("primary")}, fallbacks: []routing.Fallback{{Route: "backup", On: []string{routing.FallbackContextWindow}}}, authority: allowed, surface: "openai", available: true, wantModes: []string{"unary", "streaming"}, enabled: true},
		{name: "forbidden context fallback", targets: []routing.Target{target("primary")}, fallbacks: []routing.Fallback{{Route: "backup", On: []string{routing.FallbackContextWindow}}}, authority: forbiddenBackup, surface: "openai", available: true, wantModes: []string{}},
		{name: "non-context fallback", targets: []routing.Target{target("primary")}, fallbacks: []routing.Fallback{{Route: "backup", On: []string{routing.FallbackExhausted}}}, authority: allowed, surface: "openai", available: true, wantModes: []string{}},
		{name: "fallback without context bound", targets: []routing.Target{target("unbounded")}, fallbacks: []routing.Fallback{{Route: "backup", On: []string{routing.FallbackContextWindow}}}, authority: allowed, surface: "openai", available: true, wantModes: []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			primary := route("workflow", test.targets, routing.Behavior{ToolExecution: "gateway", Toolsets: []string{testToolset}, Selectors: test.selectors, Fallbacks: test.fallbacks})
			release := &runtime.Release{Snapshot: &routing.Snapshot{Providers: providers, Routes: map[string]routing.Route{"workflow": primary, "backup": backup}}}
			capability := DescribeReachableWorkflow(t.Context(), catalog, test.authority, release, primary, test.surface, test.available, time.Now())
			if capability == nil || capability.Enabled != test.enabled || !slices.Equal(capability.CertifiedModes, test.wantModes) {
				t.Fatalf("capability = %+v, want enabled %v and modes %v", capability, test.enabled, test.wantModes)
			}
			if !capability.DefaultToolsetsAllowed || len(capability.Tools) != 1 || capability.Tools[0].Classification != "read_only" {
				t.Fatalf("authorized tool not described: %+v", capability)
			}
		})
	}
}
