package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
)

func (s *Server) executeToolLoop(ctx context.Context, x *Call) *Outcome {
	first, failure := s.resumeToolLoop(ctx, x)
	if failure != nil {
		return &Outcome{Err: failure}
	}
	for iteration := first; int64(iteration) < *x.generation.runLimits.MaxIterations; iteration++ {
		out, frames, failure := s.modelTurn(ctx, x, iteration)
		if failure != nil {
			return &Outcome{Err: failure}
		}
		if out.Err != nil || out.Completion == nil || len(out.Completion.ToolCalls) == 0 {
			if failure := s.finalAnswer(x, out, frames); failure != nil {
				return &Outcome{Err: failure}
			}
			return out
		}
		paused, failure := s.toolTurn(ctx, x, out.Completion, iteration)
		if failure != nil {
			return &Outcome{Err: failure}
		}
		if paused != nil {
			return paused
		}
	}
	return &Outcome{Err: fault.InvalidRequest("tool_iteration_limit", "The workflow reached its model iteration limit.", nil)}
}

func (s *Server) resumeToolLoop(ctx context.Context, x *Call) (int, *fault.Error) {
	if x.workflow.resume == nil {
		return 0, nil
	}
	state := x.workflow.resume
	x.workflow.calls = state.Calls
	parsed, err := x.generation.parsed.WithField(x.family.WorkflowTranscriptField(), state.Transcript)
	if err != nil {
		return 0, fault.InvalidRequest("tool_approval_invalid", "The approved workflow could not be resumed.", nil)
	}
	x.generation.parsed = parsed
	if x.guard.engine != nil && x.guard.engine.HasCompression() {
		x.guard.ctx = ctx
		if _, _, err := x.inspectGuardrailInput(parsed.OIF().Document()); err != nil {
			return 0, guardrailError(err)
		}
	}
	continued, failure := s.continueToolLoop(ctx, x, state.Completion, x.workflow.resumeStart)
	if failure != nil {
		return 0, failure
	}
	x.generation.parsed = continued
	return state.Iteration + 1, nil
}

func (s *Server) modelTurn(ctx context.Context, x *Call, iteration int) (*Outcome, []byte, *fault.Error) {
	if iteration > 0 {
		parsed, failure := s.pruneWorkflow(ctx, x, x.generation.parsed)
		if failure != nil {
			return nil, nil, failure
		}
		x.generation.parsed = parsed
	}
	authority, e := s.workflowAuthority(ctx, x, x.Route, "")
	if e != nil {
		return nil, nil, e
	}
	var frames bytes.Buffer
	if x.generation.parsed.Stream {
		x.generation.emit = func(frame []byte) error {
			if int64(frames.Len()+len(frame)) > s.Config.MaxResponseBytes {
				return ErrResponseTooLarge
			}
			_, err := frames.Write(frame)
			return err
		}
	}
	out := s.executeModelStep(ctx, x, x.generation.parsed, *authority)
	if out.Err != nil || out.Completion == nil {
		return out, frames.Bytes(), nil
	}
	if x.workflow.responseMCP && !x.responseMCPContractCertified() {
		x.workflow.responseMCP = false
		x.workflow.responseItems = nil
	}
	if x.generation.parsed.Stream {
		if failure := s.readStreamedTurn(x, out.Completion, frames.Bytes()); failure != nil {
			return nil, nil, failure
		}
	}
	if !x.generation.parsed.Stream && x.family.Surface() == "gemini" && !protocols.WorkflowSingleCandidate(out.Completion.Body) {
		return nil, nil, fault.ServerError(http.StatusBadGateway, "invalid_tool_response", "The model returned multiple candidates.")
	}
	return out, frames.Bytes(), nil
}

func (s *Server) readStreamedTurn(x *Call, turn *wire.Completion, frames []byte) *fault.Error {
	invalid := fault.ServerError(http.StatusBadGateway, "invalid_tool_response", "The model returned an invalid streaming response.")
	completion, err := workflowStreamCompletion(x.family, bytes.NewReader(frames), int(s.Config.MaxEventBytes), int(s.Config.MaxResponseBytes), x.named().Slug)
	if err != nil {
		return invalid
	}
	turn.ToolCalls, turn.OutputText, turn.Refusal = completion.ToolCalls, completion.OutputText, completion.Refusal
	if len(completion.ToolCalls) == 0 || x.family == wire.Chat {
		return nil
	}
	if x.family == wire.Responses {
		turn.Body = completion.Body
		return nil
	}
	body, err := protocols.WorkflowStreamBody(x.family, bytes.NewReader(frames), int(s.Config.MaxEventBytes))
	if err != nil {
		return invalid
	}
	turn.Body = body
	return nil
}

func (s *Server) finalAnswer(x *Call, out *Outcome, frames []byte) *fault.Error {
	if out.Err != nil {
		return nil
	}
	unprepared := fault.ServerError(http.StatusBadGateway, "invalid_tool_response", "The workflow response could not be prepared.")
	if out.Completion != nil && !x.generation.parsed.Stream && len(x.workflow.responseItems) > 0 {
		body, err := projectResponseMCPItems(out.Completion.Body, x.workflow.responseItems, s.Config.MaxResponseBytes)
		if err != nil {
			return unprepared
		}
		out.Completion.Body = body
	}
	if !x.generation.parsed.Stream {
		return nil
	}
	if len(x.workflow.responseItems) == 0 {
		x.workflow.finalFrames = slices.Clone(frames)
		return nil
	}
	projected, err := projectResponseMCPFrames(frames, x.workflow.responseItems, int(s.Config.MaxEventBytes), s.Config.MaxResponseBytes)
	if err != nil {
		return unprepared
	}
	x.workflow.finalFrames = projected
	return nil
}

func (s *Server) toolTurn(ctx context.Context, x *Call, completion *wire.Completion, iteration int) (*Outcome, *fault.Error) {
	if int64(iteration)+1 == *x.generation.runLimits.MaxIterations {
		return nil, fault.InvalidRequest("tool_iteration_limit", "The workflow reached its model iteration limit.", nil)
	}
	if int64(len(completion.ToolCalls)+x.workflow.calls) > *x.generation.runLimits.MaxToolCalls {
		return nil, fault.InvalidRequest("tool_call_limit", "The workflow reached its tool call limit.", nil)
	}
	pause, failure := s.pauseWorkflow(ctx, x, completion, iteration)
	if failure != nil {
		return nil, failure
	}
	if pause != nil {
		return &Outcome{Completion: completion, approval: pause}, nil
	}
	parsed, failure := s.continueToolLoop(ctx, x, completion, x.workflow.ordinal)
	if failure != nil {
		return nil, failure
	}
	x.generation.parsed = parsed
	return nil, nil
}

func workflowStreamCompletion(family wire.Format, frames *bytes.Reader, maxEvent, maxResponse int, route string) (*wire.Completion, error) {
	if family == wire.Responses {
		completion, err := openai.Aggregate(family, frames, maxEvent, maxResponse)
		if err != nil {
			return nil, err
		}
		return openai.DecodeResponse(completion.Body, route)
	}
	if family == wire.Chat {
		return openai.Stream(wire.Chat, frames, maxEvent, route)
	}
	source := family
	if family.Surface() == "gemini" {
		source = wire.Gemini
	}
	var translated bytes.Buffer
	_, err := protocols.StreamWithEvents(source, wire.Chat, frames, maxEvent, route, true, func(frame []byte) error {
		_, err := translated.Write(frame)
		return err
	}, nil, 1)
	if err != nil {
		return nil, err
	}
	return openai.Stream(wire.Chat, &translated, maxEvent, route)
}

func (s *Server) continueToolLoop(ctx context.Context, x *Call, completion *wire.Completion, identityStart int) (*wire.Request, *fault.Error) {
	assistant, failure := assistantTurn(x, completion)
	if failure != nil {
		return nil, failure
	}
	field := x.family.WorkflowTranscriptField()
	messages, failure := workflowTranscript(x, field)
	if failure != nil {
		return nil, failure
	}
	if x.family == wire.Chat {
		messages = append(messages, assistant)
	}
	results := make([]protocols.WorkflowResult, 0, len(completion.ToolCalls))
	for callIndex, call := range completion.ToolCalls {
		result, failure := s.runToolCall(ctx, x, call, identityStart+callIndex)
		if failure != nil {
			return nil, failure
		}
		if x.family != wire.Chat {
			results = append(results, protocols.WorkflowResult{CallID: call.ID, Content: string(result)})
			continue
		}
		message, err := json.Marshal(struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			Content    string `json:"content"`
		}{Role: "tool", ToolCallID: call.ID, Content: string(result)})
		if err != nil {
			return nil, fault.ServerError(http.StatusServiceUnavailable, "tool_execution_unavailable", "The tool result could not be continued.")
		}
		messages = append(messages, message)
	}
	if x.family != wire.Chat {
		turns, err := protocols.WorkflowTurns(x.family, completion, results)
		if err != nil {
			return nil, fault.ServerError(http.StatusBadGateway, "invalid_tool_response", "The model returned an invalid tool response.")
		}
		messages = append(messages, turns...)
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		return nil, fault.ServerError(http.StatusServiceUnavailable, "tool_execution_unavailable", "The workflow could not be continued.")
	}
	parsed, err := x.generation.parsed.WithField(field, raw)
	if err != nil {
		return nil, fault.InvalidRequest("invalid_request", "The request messages could not be continued.", nil)
	}
	return parsed, nil
}

func assistantTurn(x *Call, completion *wire.Completion) (json.RawMessage, *fault.Error) {
	invalid := fault.ServerError(http.StatusBadGateway, "invalid_tool_response", "The model returned an invalid tool response.")
	if x.family != wire.Chat {
		return nil, nil
	}
	if x.generation.parsed.Stream {
		calls := make([]map[string]any, len(completion.ToolCalls))
		for i, call := range completion.ToolCalls {
			calls[i] = map[string]any{"id": call.ID, "type": "function", "function": map[string]string{"name": call.Name, "arguments": call.Arguments}}
		}
		assistant, err := json.Marshal(map[string]any{"role": "assistant", "content": completion.OutputText, "tool_calls": calls})
		if err != nil {
			return nil, invalid
		}
		return assistant, nil
	}
	var response struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(completion.Body, &response) != nil || len(response.Choices) != 1 || len(response.Choices[0].Message) == 0 {
		return nil, invalid
	}
	return response.Choices[0].Message, nil
}

func workflowTranscript(x *Call, field string) ([]json.RawMessage, *fault.Error) {
	uncontinued := fault.InvalidRequest("invalid_request", "The request messages could not be continued.", nil)
	input := x.generation.parsed.Field(field)
	if x.family == wire.Responses && len(input) > 0 && input[0] == '"' {
		message, err := json.Marshal(struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}{Role: "user", Content: input})
		if err != nil {
			return nil, uncontinued
		}
		return []json.RawMessage{message}, nil
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(input, &messages); err != nil {
		return nil, uncontinued
	}
	return messages, nil
}

func (s *Server) runToolCall(ctx context.Context, x *Call, call wire.ToolCall, identity int) (json.RawMessage, *fault.Error) {
	authority, e := s.workflowAuthority(ctx, x, x.Route, "")
	if e != nil {
		return nil, e
	}
	index := slices.IndexFunc(x.workflow.tools, func(tool WorkflowTool) bool { return tool.Definition.Alias == call.Name })
	if index < 0 || call.ID == "" || slices.Contains(x.workflowRecoveryCalls, call.ID) || !json.Valid([]byte(call.Arguments)) {
		return nil, fault.InvalidRequest("invalid_tool_call", "The model requested an unavailable tool or invalid arguments.", nil)
	}
	toolAuthority := *authority
	toolAuthority.Attribution = x.attribution
	toolCost, costKnown, costErr := x.workflowToolCost(x.workflow.tools[index])
	if costErr != nil {
		return nil, costErr
	}
	callCtx, finish := s.monitorWorkflowAuthority(ctx, x, x.workflow.tools[index].ToolsetID)
	result, err := s.callWorkflowTool(callCtx, x, toolAuthority, x.workflow.tools[index], call, identity)
	authorityErr := finish()
	x.workflow.ordinal++
	x.workflow.calls++
	if authorityErr != nil {
		return nil, authorityErr
	}
	if err != nil {
		return nil, workflowError(err)
	}
	if x.workflow.responseMCP && call.Name != recoveryToolName {
		item, err := responseMCPCall(x.workflow.tools[index], call, result)
		if err != nil {
			return nil, fault.ServerError(http.StatusBadGateway, "invalid_tool_response", "The tool returned an invalid response.")
		}
		x.workflow.responseItems = append(x.workflow.responseItems, item)
	}
	x.workflow.toolCost = x.workflow.toolCost.Add(toolCost)
	x.workflow.unknownCost = x.workflow.unknownCost || !costKnown
	return result, nil
}
