package routes

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/routing"
)

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
