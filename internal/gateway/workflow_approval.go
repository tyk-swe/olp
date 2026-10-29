package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

type workflowPauseCall struct {
	ServerLabel string          `json:"server_label,omitempty"`
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Arguments   json.RawMessage `json:"arguments"`
}

type workflowApprovalItem struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	ServerLabel string `json:"server_label"`
	Arguments   string `json:"arguments"`
}

type workflowPause struct {
	ID                string                 `json:"id,omitempty"`
	CreatedAt         int64                  `json:"created_at,omitempty"`
	Model             string                 `json:"model,omitempty"`
	Object            string                 `json:"object,omitempty"`
	Status            string                 `json:"status,omitempty"`
	Output            []workflowApprovalItem `json:"output,omitempty"`
	Type              string                 `json:"type"`
	ContinuationToken string                 `json:"continuation_token"`
	Calls             []workflowPauseCall    `json:"calls"`
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
	if json.Unmarshal(continuation.State, &state) != nil || state.Family != x.family || state.Completion == nil || state.Iteration < 0 || state.Iteration >= 7 || state.Ordinal < 0 || state.Ordinal != continuation.StartOrdinal || continuation.NextOrdinal < state.Ordinal || state.Calls < 0 || state.Calls > 32 {
		return permissionError("tool_approval_invalid", "The workflow approval is invalid or expired.")
	}
	parsed, err := x.generation.parsed.WithResourceBindings(oif.Change{Pointer: oif.Pointer("", "olp_approval"), Remove: true, Reason: "workflow approval control"})
	if err != nil {
		return invalidRequest("invalid_request", "The workflow approval could not be removed from the request.", nil)
	}
	x.generation.parsed = parsed
	if x.deadline.IsZero() || continuation.ExpiresAt.Before(x.deadline) {
		x.deadline = continuation.ExpiresAt
	}
	x.workflow.runID = continuation.RunID
	state.Ordinal = continuation.NextOrdinal
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
	for _, call := range completion.ToolCalls {
		index := slices.IndexFunc(x.workflow.tools, func(tool WorkflowTool) bool { return tool.Definition.Alias == call.Name })
		if index < 0 || call.ID == "" || !json.Valid([]byte(call.Arguments)) {
			return nil, invalidRequest("invalid_tool_call", "The model requested an unavailable tool or invalid arguments.", nil)
		}
		tool := x.workflow.tools[index]
		if tool.Definition.Classification != "read_only" && tool.Definition.UnitPrice == nil {
			return nil, invalidRequest("tool_approval_unavailable", "The tool does not have a finite approval allowance.", nil)
		}
	}
	type candidate struct {
		request agents.WorkflowApprovalCall
		call    workflowPauseCall
	}
	candidates := slices.Collect(func(yield func(candidate) bool) {
		for _, call := range completion.ToolCalls {
			index := slices.IndexFunc(x.workflow.tools, func(tool WorkflowTool) bool { return tool.Definition.Alias == call.Name })
			tool := x.workflow.tools[index]
			if tool.Definition.Classification == "read_only" {
				continue
			}
			if !yield(candidate{
				request: agents.WorkflowApprovalCall{CallID: call.ID, ToolsetID: tool.ToolsetID, ToolsetRevisionID: tool.RevisionID, ToolAlias: call.Name, Arguments: json.RawMessage(call.Arguments), MaxCost: *tool.Definition.UnitPrice},
				call:    workflowPauseCall{ID: call.ID, Name: call.Name, Arguments: json.RawMessage(call.Arguments), ServerLabel: tool.ToolsetID},
			}) {
				return
			}
		}
	})
	requests := slices.Collect(func(yield func(agents.WorkflowApprovalCall) bool) {
		for _, candidate := range candidates {
			if !yield(candidate.request) {
				return
			}
		}
	})
	if len(requests) == 0 {
		return nil, nil
	}
	calls := slices.Collect(func(yield func(workflowPauseCall) bool) {
		for _, candidate := range candidates {
			if !yield(candidate.call) {
				return
			}
		}
	})
	store, ok := s.WorkflowRuns.(WorkflowApprovalStore)
	if !ok {
		return nil, serverError(503, "tool_approval_unavailable", "The approval service is unavailable.")
	}
	state, err := json.Marshal(workflowResumeState{Family: x.family, Transcript: x.generation.parsed.Field(workflowTranscriptField(x.family)), Completion: completion, Iteration: iteration, Ordinal: x.workflow.ordinal, Calls: x.workflow.calls})
	if err != nil {
		return nil, serverError(503, "tool_approval_unavailable", "The approval could not be prepared.")
	}
	deadline, _ := ctx.Deadline()
	token, err := store.Pause(ctx, x.authority, agents.WorkflowApprovalRequest{RunID: x.workflow.runID, RouteRevisionID: x.named().RevisionID, StartOrdinal: x.workflow.ordinal, Transcript: x.workflow.transcript, State: state, Calls: requests, ExpiresAt: deadline}, int(s.cfg.MaxBodyBytes))
	if err != nil {
		return nil, serverError(503, "tool_approval_unavailable", "The approval could not be prepared.")
	}
	if !x.workflow.responseMCP {
		return &workflowPause{Type: "tool_approval_required", ContinuationToken: token, Calls: calls}, nil
	}
	output := slices.Collect(func(yield func(workflowApprovalItem) bool) {
		for _, call := range calls {
			if !yield(workflowApprovalItem{Type: "mcp_approval_request", ID: call.ID, Name: call.Name, ServerLabel: call.ServerLabel, Arguments: string(call.Arguments)}) {
				return
			}
		}
	})
	return &workflowPause{ID: "resp_" + x.request.accountingID(), CreatedAt: time.Now().Unix(), Model: x.named().Slug, Object: "response", Status: "incomplete", Output: output, Type: "tool_approval_required", ContinuationToken: token, Calls: calls}, nil
}

func workflowApprovalFrames(pause *workflowPause, maxEvent, maxResponse int) ([]byte, error) {
	// Frames must be buffered in order to enforce the response-size limit before delivery.
	var output bytes.Buffer
	sequence := 0
	for index, item := range pause.Output {
		for _, kind := range []string{"response.output_item.added", "response.output_item.done"} {
			payload, err := json.Marshal(struct {
				Type        string               `json:"type"`
				Sequence    int                  `json:"sequence_number"`
				OutputIndex int                  `json:"output_index"`
				Item        workflowApprovalItem `json:"item"`
			}{Type: kind, Sequence: sequence, OutputIndex: index, Item: item})
			if err != nil || len(payload) > maxEvent {
				return nil, errResponseTooLarge
			}
			frame := sse.Frame{Event: &kind, Data: string(payload)}.Encode()
			if output.Len()+len(frame) > maxResponse {
				return nil, errResponseTooLarge
			}
			output.Write(frame)
			sequence++
		}
	}
	kind := "response.incomplete"
	payload, err := json.Marshal(struct {
		Type     string         `json:"type"`
		Sequence int            `json:"sequence_number"`
		Response *workflowPause `json:"response"`
	}{Type: kind, Sequence: sequence, Response: pause})
	if err != nil || len(payload) > maxEvent {
		return nil, errResponseTooLarge
	}
	frame := sse.Frame{Event: &kind, Data: string(payload)}.Encode()
	if output.Len()+len(frame) > maxResponse {
		return nil, errResponseTooLarge
	}
	output.Write(frame)
	return output.Bytes(), nil
}
