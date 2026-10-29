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
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
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
	RunLimits     limits.WorkflowBounds `json:"run_limits"`
	PriorCost     string                `json:"prior_cost"`
	CostKnown     bool                  `json:"cost_known"`
	StartedAt     time.Time             `json:"started_at"`
	Family        wire.Format           `json:"family"`
	Transcript    json.RawMessage       `json:"transcript"`
	Completion    *wire.Completion      `json:"completion"`
	Iteration     int                   `json:"iteration"`
	Calls         int                   `json:"calls"`
	ResponseMCP   bool                  `json:"response_mcp"`
	ResponseItems []responseMCPItem     `json:"response_items,omitempty"`
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
	for _, approval := range continuation.Approvals {
		if !slices.ContainsFunc(x.workflow.tools, func(tool WorkflowTool) bool {
			return tool.ToolsetID == approval.Binding.ToolsetID && tool.RevisionID == approval.Binding.ToolsetRevisionID && tool.Definition.Alias == approval.Binding.ToolAlias
		}) {
			return permissionError("tool_approval_invalid", "The workflow approval is invalid or expired.")
		}
	}
	var state workflowResumeState
	if json.Unmarshal(continuation.State, &state) != nil || state.Family != x.family || state.Completion == nil || state.Iteration < 0 || state.Iteration >= 31 || continuation.StartOrdinal < 0 || continuation.NextOrdinal < continuation.StartOrdinal || state.Calls < 0 || state.Calls > 128 || !state.RunLimits.Valid() || state.RunLimits.MaxIterations == nil || state.RunLimits.MaxToolCalls == nil || state.RunLimits.MaxDurationSeconds == nil || state.StartedAt.IsZero() {
		return permissionError("tool_approval_invalid", "The workflow approval is invalid or expired.")
	}
	priorCost, validCost := usage.ParseCost(state.PriorCost)
	if !validCost {
		return permissionError("tool_approval_invalid", "The workflow approval is invalid or expired.")
	}
	bounds := resolveWorkflowBounds(&state.RunLimits, x.named().RunLimits, x.authority.Policy.RunLimits, &x.generation.callerRunLimits)
	if int64(state.Iteration)+1 >= *bounds.MaxIterations {
		return invalidRequest("tool_iteration_limit", "The workflow reached its model iteration limit.", nil)
	}
	if int64(state.Calls+len(state.Completion.ToolCalls)) > *bounds.MaxToolCalls {
		return invalidRequest("tool_call_limit", "The workflow reached its tool call limit.", nil)
	}
	if bounds.MaxRunCost != nil && !state.CostKnown {
		return workflowUnboundedCost()
	}
	x.generation.runLimits = bounds
	x.workflow.priorCost = priorCost
	x.workflow.unknownCost = !state.CostKnown
	x.workflow.startedAt = state.StartedAt
	deadline := state.StartedAt.Add(time.Duration(*bounds.MaxDurationSeconds) * time.Second)
	if x.deadline.IsZero() || deadline.Before(x.deadline) {
		x.deadline = deadline
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
	x.workflow.ordinal = continuation.NextOrdinal
	x.workflow.resumeStart = continuation.StartOrdinal
	x.workflow.resume = &state
	x.workflow.approvals = continuation.Approvals
	x.workflow.responseMCP = x.workflow.responseMCP && state.ResponseMCP
	if x.workflow.responseMCP {
		x.workflow.responseItems = state.ResponseItems
	} else {
		x.workflow.responseItems = nil
	}
	return nil
}

func (s *Server) callWorkflowTool(ctx context.Context, x *call, authority access.Authority, tool WorkflowTool, call wire.ToolCall, identityOrdinal int) (json.RawMessage, error) {
	callID := fmt.Sprintf("%s:%d:%s", x.workflow.runID, identityOrdinal, call.ID)
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
			maxCost := func() string {
				if tool.Definition.UnitPrice == nil {
					return "0"
				}
				return *tool.Definition.UnitPrice
			}()
			if !yield(candidate{
				request: agents.WorkflowApprovalCall{CallID: call.ID, ToolsetID: tool.ToolsetID, ToolsetRevisionID: tool.RevisionID, ToolAlias: call.Name, Arguments: json.RawMessage(call.Arguments), MaxCost: maxCost},
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
	_, modelCostKnown := x.responseCost()
	costKnown := modelCostKnown && !x.workflow.unknownCost && !slices.ContainsFunc(x.facts, func(fact AttemptFact) bool {
		return fact.BillingUncertain && !fact.UsageObserved
	})
	startedAt := func() time.Time {
		if x.workflow.startedAt.IsZero() {
			return x.request.startedAt
		}
		return x.workflow.startedAt
	}()
	priorCost := x.workflow.priorCost.Add(x.settledCost()).Add(x.workflow.toolCost)
	state, err := json.Marshal(workflowResumeState{RunLimits: x.generation.runLimits, PriorCost: priorCost.String(), CostKnown: costKnown, StartedAt: startedAt, Family: x.family, Transcript: x.generation.parsed.Field(workflowTranscriptField(x.family)), Completion: completion, Iteration: iteration, Calls: x.workflow.calls, ResponseMCP: x.workflow.responseMCP, ResponseItems: x.workflow.responseItems})
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

func workflowPauseFrames(family wire.Format, model, id string, pause *workflowPause, maxEvent, maxResponse int) ([]byte, error) {
	if len(pause.Output) > 0 {
		return workflowApprovalFrames(pause, maxEvent, maxResponse)
	}
	type event struct {
		name string
		data json.RawMessage
	}
	type chatDelta struct {
		Role string `json:"role"`
	}
	type chatChoice struct {
		Index        int       `json:"index"`
		Delta        chatDelta `json:"delta"`
		FinishReason string    `json:"finish_reason"`
	}
	type chatEvent struct {
		ID       string         `json:"id"`
		Object   string         `json:"object"`
		Created  int64          `json:"created"`
		Model    string         `json:"model"`
		Choices  []chatChoice   `json:"choices"`
		Approval *workflowPause `json:"olp_approval"`
	}
	type messageUsage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	type message struct {
		ID      string            `json:"id"`
		Type    string            `json:"type"`
		Role    string            `json:"role"`
		Model   string            `json:"model"`
		Content []json.RawMessage `json:"content"`
		Usage   messageUsage      `json:"usage"`
	}
	type messageStart struct {
		Type    string  `json:"type"`
		Message message `json:"message"`
	}
	type messageDeltaFields struct {
		StopReason   string  `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	}
	type messageDelta struct {
		Type     string             `json:"type"`
		Delta    messageDeltaFields `json:"delta"`
		Usage    messageUsage       `json:"usage"`
		Approval *workflowPause     `json:"olp_approval"`
	}
	type messageStop struct {
		Type string `json:"type"`
	}
	type response struct {
		ID        string            `json:"id"`
		Object    string            `json:"object"`
		CreatedAt int64             `json:"created_at"`
		Model     string            `json:"model"`
		Status    string            `json:"status"`
		Output    []json.RawMessage `json:"output"`
		Approval  *workflowPause    `json:"olp_approval"`
	}
	type responseEvent struct {
		Type     string   `json:"type"`
		Sequence int      `json:"sequence_number"`
		Response response `json:"response"`
	}
	type geminiContent struct {
		Role  string            `json:"role"`
		Parts []json.RawMessage `json:"parts"`
	}
	type geminiCandidate struct {
		Index        int           `json:"index"`
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	}
	type geminiEvent struct {
		Candidates []geminiCandidate `json:"candidates"`
		Approval   *workflowPause    `json:"olp_approval"`
	}

	events, err := func() ([]event, error) {
		switch {
		case family == wire.Chat:
			data, err := json.Marshal(chatEvent{ID: "chatcmpl_" + id, Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: model, Choices: []chatChoice{{Delta: chatDelta{Role: "assistant"}, FinishReason: "stop"}}, Approval: pause})
			return []event{{data: data}, {data: json.RawMessage("[DONE]")}}, err
		case family == wire.Anthropic:
			start, err := json.Marshal(messageStart{Type: "message_start", Message: message{ID: "msg_" + id, Type: "message", Role: "assistant", Model: model, Content: []json.RawMessage{}}})
			if err != nil {
				return nil, err
			}
			delta, err := json.Marshal(messageDelta{Type: "message_delta", Delta: messageDeltaFields{StopReason: "end_turn"}, Approval: pause})
			if err != nil {
				return nil, err
			}
			stop, err := json.Marshal(messageStop{Type: "message_stop"})
			return []event{{name: "message_start", data: start}, {name: "message_delta", data: delta}, {name: "message_stop", data: stop}}, err
		case family == wire.Responses:
			data, err := json.Marshal(responseEvent{Type: "response.incomplete", Response: response{ID: "resp_" + id, Object: "response", CreatedAt: time.Now().Unix(), Model: model, Status: "incomplete", Output: []json.RawMessage{}, Approval: pause}})
			return []event{{name: "response.incomplete", data: data}}, err
		case family.Surface() == "gemini":
			data, err := json.Marshal(geminiEvent{Candidates: []geminiCandidate{{Content: geminiContent{Role: "model", Parts: []json.RawMessage{}}, FinishReason: "STOP"}}, Approval: pause})
			return []event{{data: data}}, err
		default:
			return nil, errResponseTooLarge
		}
	}()
	if err != nil {
		return nil, errResponseTooLarge
	}
	var output bytes.Buffer
	for _, event := range events {
		if len(event.data) > maxEvent {
			return nil, errResponseTooLarge
		}
		frame := sse.Frame{Data: string(event.data)}
		if event.name != "" {
			frame.Event = &event.name
		}
		encoded := frame.Encode()
		if output.Len()+len(encoded) > maxResponse {
			return nil, errResponseTooLarge
		}
		output.Write(encoded)
	}
	return output.Bytes(), nil
}
