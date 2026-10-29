package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

type workflowPauseCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type workflowPause struct {
	Type              string              `json:"type"`
	ContinuationToken string              `json:"continuation_token"`
	Calls             []workflowPauseCall `json:"calls"`
}

type workflowResumeState struct {
	Family     wire.Format      `json:"family"`
	Transcript json.RawMessage  `json:"transcript"`
	Completion *wire.Completion `json:"completion"`
	Iteration  int              `json:"iteration"`
	Ordinal    int              `json:"ordinal"`
	Calls      int              `json:"calls"`
}

type workflowResumeRequest struct {
	ContinuationToken string                     `json:"continuation_token"`
	Arguments         map[string]json.RawMessage `json:"arguments"`
	Approved          bool                       `json:"approved"`
}

func (s *Server) prepareWorkflowResume(ctx context.Context, x *call) *Error {
	raw := x.generation.parsed.Field("olp_approval")
	if len(raw) == 0 {
		return nil
	}
	var request workflowResumeRequest
	if json.Unmarshal(raw, &request) != nil || !request.Approved || request.ContinuationToken == "" || len(request.Arguments) == 0 {
		return permissionError("tool_approval_invalid", "The workflow approval is invalid or expired.")
	}
	store, ok := s.WorkflowRuns.(WorkflowApprovalStore)
	if !ok {
		return serverError(503, "tool_approval_unavailable", "The approval service is unavailable.")
	}
	continuation, err := store.Continue(ctx, x.authority, request.ContinuationToken, x.workflow.transcript, request.Arguments, int(s.cfg.MaxBodyBytes))
	if err != nil || continuation.RouteRevisionID != x.named().RevisionID {
		return permissionError("tool_approval_invalid", "The workflow approval is invalid or expired.")
	}
	var state workflowResumeState
	if json.Unmarshal(continuation.State, &state) != nil || state.Family != x.family || state.Completion == nil || state.Iteration < 0 || state.Iteration >= 7 || state.Ordinal < 0 || state.Calls < 0 || state.Calls > 32 {
		return permissionError("tool_approval_invalid", "The workflow approval is invalid or expired.")
	}
	parsed, err := x.generation.parsed.WithResourceBindings(oif.Change{Pointer: oif.Pointer("", "olp_approval"), Remove: true, Reason: "workflow approval control"})
	if err != nil {
		return invalidRequest("invalid_request", "The workflow approval could not be removed from the request.", nil)
	}
	x.generation.parsed = parsed
	x.workflow.runID = continuation.RunID
	x.workflow.resume = &state
	x.workflow.approvals = continuation.Approvals
	return nil
}

func (s *Server) callWorkflowTool(ctx context.Context, x *call, authority access.Authority, tool WorkflowTool, call wire.ToolCall) (json.RawMessage, error) {
	callID := fmt.Sprintf("%s:%d:%s", x.workflow.runID, x.workflow.ordinal, call.ID)
	index := slices.IndexFunc(x.workflow.approvals, func(approval agents.WorkflowApproval) bool { return approval.CallID == call.ID })
	if index < 0 {
		return s.WorkflowTools.Call(ctx, authority, x.request.release, tool, x.workflow.runID, x.workflow.ordinal, callID, json.RawMessage(call.Arguments))
	}
	runner, ok := s.WorkflowTools.(WorkflowApprovedToolRunner)
	if !ok {
		return nil, &mcp.WorkflowError{Status: 503, Code: "tool_approval_unavailable", Message: "The approval service is unavailable."}
	}
	return runner.CallApproved(ctx, authority, x.request.release, tool, x.workflow.runID, x.workflow.ordinal, callID, json.RawMessage(call.Arguments), x.workflow.approvals[index])
}

func (s *Server) pauseWorkflow(ctx context.Context, x *call, completion *wire.Completion, iteration int) (*workflowPause, *Error) {
	requests := make([]agents.WorkflowApprovalCall, 0, len(completion.ToolCalls))
	calls := make([]workflowPauseCall, 0, len(completion.ToolCalls))
	for _, call := range completion.ToolCalls {
		index := slices.IndexFunc(x.workflow.tools, func(tool WorkflowTool) bool { return tool.Definition.Alias == call.Name })
		if index < 0 || call.ID == "" || !json.Valid([]byte(call.Arguments)) {
			return nil, invalidRequest("invalid_tool_call", "The model requested an unavailable tool or invalid arguments.", nil)
		}
		tool := x.workflow.tools[index]
		if tool.Definition.Classification == "read_only" {
			continue
		}
		if tool.Definition.UnitPrice == nil {
			return nil, invalidRequest("tool_approval_unavailable", "The tool does not have a finite approval allowance.", nil)
		}
		requests = append(requests, agents.WorkflowApprovalCall{CallID: call.ID, ToolsetID: tool.ToolsetID, ToolsetRevisionID: tool.RevisionID, ToolAlias: call.Name, Arguments: json.RawMessage(call.Arguments), MaxCost: *tool.Definition.UnitPrice})
		calls = append(calls, workflowPauseCall{ID: call.ID, Name: call.Name, Arguments: json.RawMessage(call.Arguments)})
	}
	if len(requests) == 0 {
		return nil, nil
	}
	store, ok := s.WorkflowRuns.(WorkflowApprovalStore)
	if !ok {
		return nil, serverError(503, "tool_approval_unavailable", "The approval service is unavailable.")
	}
	state, err := json.Marshal(workflowResumeState{Family: x.family, Transcript: x.generation.parsed.Field(workflowTranscriptField(x.family)), Completion: completion, Iteration: iteration, Ordinal: x.workflow.ordinal, Calls: x.workflow.calls})
	if err != nil {
		return nil, serverError(503, "tool_approval_unavailable", "The approval could not be prepared.")
	}
	deadline, _ := ctx.Deadline()
	token, err := store.Pause(ctx, x.authority, agents.WorkflowApprovalRequest{RunID: x.workflow.runID, RouteRevisionID: x.named().RevisionID, Transcript: x.workflow.transcript, State: state, Calls: requests, ExpiresAt: deadline}, int(s.cfg.MaxBodyBytes))
	if err != nil {
		return nil, serverError(503, "tool_approval_unavailable", "The approval could not be prepared.")
	}
	return &workflowPause{Type: "tool_approval_required", ContinuationToken: token, Calls: calls}, nil
}
