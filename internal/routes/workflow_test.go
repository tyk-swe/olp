package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

type emptyWorkflowResolver struct{}

func (emptyWorkflowResolver) Resolve(context.Context, access.Authority, *runtime.Release, []string) ([]mcp.WorkflowTool, error) {
	return nil, nil
}

func TestWorkflowSimulationChecksFallbackAuthorization(t *testing.T) {
	projectID, keyID := uuid.NewString(), uuid.NewString()
	snapshot, _ := estimateSnapshot("model")
	primary := snapshot.Routes["route"]
	primary.ProjectID, primary.ToolExecution = &projectID, "gateway"
	backup := primary
	backup.ID, backup.RoutingID, backup.Slug = uuid.NewString(), uuid.NewString(), "backup"
	provider := snapshot.Providers[primary.Targets[0].ProviderID]
	provider.Capabilities[0].ToolCalls = true
	snapshot.Providers[provider.ID] = provider
	primary.Targets = append([]routing.Target(nil), primary.Targets...)
	tooSmall := provider
	tooSmall.ID = uuid.NewString()
	tooSmall.Models = map[string]json.RawMessage{"model": json.RawMessage(`{"context_length":1}`)}
	snapshot.Providers[tooSmall.ID] = tooSmall
	primary.Targets[0].ProviderID = tooSmall.ID
	primary.Fallbacks = []routing.Fallback{{Route: "backup", On: []string{routing.FallbackContextWindow}}}
	snapshot.Routes["route"], snapshot.Routes["backup"] = primary, backup
	input := simulationInput{Operation: "generation", Surface: "openai", Mode: "unary", Dialect: "openai-chat", Request: json.RawMessage(`{"model":"route","messages":[{"role":"user","content":"hello"}],"max_tokens":20}`)}
	m := fixedSimulation(snapshot, input, &routing.TokenDemand{EstimatedInputTokens: 2})
	m.key = func(route routing.Route) (inspectionKeyContext, error) {
		if route.Slug == "backup" {
			return inspectionKeyContext{reason: "key_route"}, nil
		}
		return inspectionKeyContext{id: keyID, authority: access.Authority{ID: keyID, ProjectID: &projectID, Policy: access.KeyPolicy{Scopes: []string{"inference", "tools"}, AllowedRoutes: []string{"route"}}}}, nil
	}
	server := &Server{Access: &access.Server{}, WorkflowTools: func() mcp.WorkflowResolver { return emptyWorkflowResolver{} }, WorkflowRelease: func() *runtime.Release { return &runtime.Release{Snapshot: snapshot} }, WorkflowAvailable: func() bool { return true }}
	out, err := server.explain(m, "route")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Legs) != 1 || len(out.Fallbacks) != 1 || out.Fallbacks[0].Outcome != routing.FallbackForbidden {
		t.Fatalf("unauthorized fallback was inspected as available: %+v", out)
	}
}

func TestWorkflowSimulationUsesPreparedOutputLimits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dialect string
		request string
	}{
		{name: "profile default", dialect: "openai-chat", request: `{"model":"route","messages":[{"role":"user","content":"hello"}],"olp_toolsets":[],"olp_run_limits":{"max_run_cost":%q}}`},
		{name: "translated Responses", dialect: "openai-responses", request: `{"model":"route","input":"hello","max_output_tokens":1000,"olp_toolsets":[],"olp_run_limits":{"max_run_cost":%q}}`},
	} {
		for _, limit := range []string{"0.5", "2"} {
			t.Run(tc.name+"/"+limit, func(t *testing.T) {
				projectID, keyID := uuid.NewString(), uuid.NewString()
				snapshot, _ := estimateSnapshot("model")
				route := snapshot.Routes["route"]
				route.ProjectID, route.ToolExecution = &projectID, "gateway"
				snapshot.Routes["route"] = route
				for id, provider := range snapshot.Providers {
					provider.Kind, provider.ProfileID, provider.ProfileRevision = "openai_compatible", "compatible-chat", "1"
					provider.Models = map[string]json.RawMessage{"model": json.RawMessage(`{"context_length":10000}`)}
					provider.Capabilities[0].ToolCalls = true
					provider.OperationDefaults = map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"max_tokens": json.RawMessage(`1000`)}}}
					snapshot.Providers[id] = provider
				}
				input := simulationInput{Operation: "generation", Surface: "openai", Mode: "unary", Dialect: tc.dialect, Request: json.RawMessage(fmt.Sprintf(tc.request, limit))}
				m := fixedSimulation(snapshot, input, nil)
				m.key = func(routing.Route) (inspectionKeyContext, error) {
					return inspectionKeyContext{id: keyID, authority: access.Authority{ID: keyID, ProjectID: &projectID, Policy: access.KeyPolicy{Scopes: []string{"inference", "tools"}}}}, nil
				}
				m.inputs = &usage.RoutingInputs{RefreshedAt: time.Now(), Prices: []usage.RoutingPrice{{Price: usage.Price{ProviderKind: "openai_compatible", Model: "model", Operation: "generation", InputPerMillion: new("0"), OutputPerMillion: new("1000")}}}}
				server := &Server{Access: &access.Server{}, WorkflowTools: func() mcp.WorkflowResolver { return emptyWorkflowResolver{} }, WorkflowRelease: func() *runtime.Release { return &runtime.Release{Snapshot: snapshot} }, WorkflowAvailable: func() bool { return true }}
				leg, _, _, err := server.leg(m, "route", "")
				if err != nil {
					t.Fatal(err)
				}
				decision := decisionFor(t, leg.Decisions, "model")
				if decision.Eligible != (limit == "2") {
					t.Fatalf("limit %s produced decision %+v", limit, decision)
				}
				if limit == "0.5" && (decision.Incompatibility == nil || decision.Incompatibility.Code != "run_cost_limit") {
					t.Fatalf("prepared output cost was not applied: %+v", decision)
				}
			})
		}
	}
}

func TestWorkflowSimulationKeepsNamedRoutePolicyAcrossServingRoutes(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		for _, tc := range []struct {
			name            string
			namedWorkflow   bool
			servingWorkflow bool
			strictServing   bool
		}{
			{name: "workflow named route", namedWorkflow: true},
			{name: "ordinary named route", servingWorkflow: true},
			{name: "strict serving route", namedWorkflow: true, strictServing: true},
		} {
			path := "selector"
			if fallback {
				path = "fallback"
			}
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				projectID, keyID := uuid.NewString(), uuid.NewString()
				snapshot, _ := estimateSnapshot("model")
				for id, provider := range snapshot.Providers {
					provider.Capabilities[0].ToolCalls = true
					snapshot.Providers[id] = provider
				}
				primary := snapshot.Routes["route"]
				primary.ProjectID = &projectID
				backup := primary
				backup.ID, backup.RoutingID, backup.Slug = uuid.NewString(), uuid.NewString(), "backup"
				if tc.namedWorkflow {
					primary.ToolExecution = "gateway"
				}
				if tc.servingWorkflow {
					backup.ToolExecution = "gateway"
				}
				if tc.strictServing {
					backup.Fidelity = routing.RouteFidelity{Mode: routing.FidelityStrict}
					backup.Fallbacks = []routing.Fallback{{Route: "third", On: []string{routing.FallbackContextWindow}}}
					third := primary
					third.ID, third.RoutingID, third.Slug = uuid.NewString(), uuid.NewString(), "third"
					snapshot.Routes["third"] = third
				}
				var demand *routing.TokenDemand
				if fallback {
					primary.Targets = append([]routing.Target(nil), primary.Targets...)
					provider := snapshot.Providers[primary.Targets[0].ProviderID]
					provider.ID = uuid.NewString()
					provider.Models = map[string]json.RawMessage{"model": json.RawMessage(`{"context_length":1}`)}
					snapshot.Providers[provider.ID] = provider
					primary.Targets[0].ProviderID = provider.ID
					primary.Fallbacks = []routing.Fallback{{Route: "backup", On: []string{routing.FallbackContextWindow}}}
					demand = &routing.TokenDemand{EstimatedInputTokens: 2}
				} else {
					primary.Selectors = []routing.Selector{{ID: "delegate", Route: "backup"}}
				}
				snapshot.Routes["route"], snapshot.Routes["backup"] = primary, backup
				input := simulationInput{Operation: "generation", Surface: "openai", Mode: "unary", Dialect: "openai-chat", Request: json.RawMessage(`{"model":"route","messages":[{"role":"user","content":"hello"}],"max_tokens":20}`)}
				m := fixedSimulation(snapshot, input, demand)
				m.key = func(routing.Route) (inspectionKeyContext, error) {
					return inspectionKeyContext{id: keyID, authority: access.Authority{ID: keyID, ProjectID: &projectID, Policy: access.KeyPolicy{Scopes: []string{"inference", "tools"}}}}, nil
				}
				server := &Server{Access: &access.Server{}, WorkflowTools: func() mcp.WorkflowResolver { return emptyWorkflowResolver{} }, WorkflowRelease: func() *runtime.Release { return &runtime.Release{Snapshot: snapshot} }, WorkflowAvailable: func() bool { return true }}
				out, err := server.explain(m, "route")
				if err != nil {
					t.Fatal(err)
				}
				if len(out.Legs) != 2 || out.Legs[1].Route != "backup" {
					t.Fatalf("serving route was not inspected correctly: %+v", out)
				}
				decision := decisionFor(t, out.Legs[1].Decisions, "model")
				if decision.Eligible == tc.strictServing {
					t.Fatalf("strict=%t produced decision %+v", tc.strictServing, decision)
				}
				if !tc.strictServing && (out.Legs[1].GatewayExecution != nil) != tc.namedWorkflow {
					t.Fatalf("serving route changed named workflow policy: %+v", out.Legs[1])
				}
				if tc.strictServing && fallback && (len(out.Fallbacks) != 1 || out.Fallbacks[0].Outcome == routing.FallbackPlanned) {
					t.Fatalf("refused route was treated as a planned fallback: %+v", out.Fallbacks)
				}
			})
		}
	}
}

type workflowDraftQuery struct {
	access.Queryer
	providerID string
}

func (q workflowDraftQuery) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	if strings.Contains(query, "FROM olp.provider_models") {
		return inspectionKeyRow{q.providerID, "provider", "model", nil}
	}
	return inspectionKeyRow{false}
}

func TestValidateWorkflowDraftPreservesToolsetPolicy(t *testing.T) {
	modelID, providerID, toolsetID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	query := workflowDraftQuery{providerID: providerID}
	for _, tc := range []struct {
		name      string
		execution string
		toolsets  []string
		fidelity  json.RawMessage
		mcpItems  bool
		runLimits *limits.WorkflowBounds
		valid     bool
	}{
		{name: "gateway workflow", execution: "gateway", toolsets: []string{toolsetID}, valid: true},
		{name: "configured workflow bounds", execution: "gateway", toolsets: []string{toolsetID}, runLimits: &limits.WorkflowBounds{MaxIterations: new(int64(7)), MaxRunCost: new("1.25")}, valid: true},
		{name: "iteration bound exceeds ceiling", execution: "gateway", toolsets: []string{toolsetID}, runLimits: &limits.WorkflowBounds{MaxIterations: new(int64(33))}},
		{name: "workflow bounds without gateway execution", runLimits: &limits.WorkflowBounds{MaxIterations: new(int64(2))}},
		{name: "Responses MCP output", execution: "gateway", toolsets: []string{toolsetID}, fidelity: json.RawMessage(`{"mode":"transformed"}`), mcpItems: true, valid: true},
		{name: "Responses MCP output on strict route", execution: "gateway", toolsets: []string{toolsetID}, mcpItems: true},
		{name: "Responses MCP output without gateway execution", fidelity: json.RawMessage(`{"mode":"transformed"}`), mcpItems: true},
		{name: "duplicate toolset", execution: "gateway", toolsets: []string{toolsetID, toolsetID}},
		{name: "unknown toolset", execution: "gateway", toolsets: []string{"not-a-uuid"}},
		{name: "toolset without gateway execution", toolsets: []string{toolsetID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := DraftInput{
				Slug: "workflow", Operations: []string{"generation"}, OverallTimeoutMS: 1000, MaxAttempts: 1,
				ToolExecution: tc.execution, Toolsets: tc.toolsets, Fidelity: tc.fidelity, ResponsesMCPItems: tc.mcpItems, RunLimits: tc.runLimits,
				Targets: []TargetInput{{ProviderModelID: &modelID, Weight: 1, TimeoutMS: 1000}},
			}
			_, err := ValidateDraftInput(t.Context(), query, &input, nil, nil)
			if !tc.valid {
				if err == nil {
					t.Fatal("invalid workflow configuration was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			stored, decodeErr := routing.DecodeBehavior(input.Behavior)
			if decodeErr != nil || stored.ToolExecution != tc.execution || stored.ResponsesMCPItems != tc.mcpItems || !reflect.DeepEqual(stored.RunLimits, tc.runLimits) || len(stored.Toolsets) != 1 || stored.Toolsets[0] != toolsetID {
				t.Fatalf("workflow policy did not survive validation: %+v, %v", stored, decodeErr)
			}
		})
	}
}
