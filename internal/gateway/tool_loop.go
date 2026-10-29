package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
)

type WorkflowRunStore interface {
	Start(context.Context, access.Authority, string, string) (string, error)
	Prepare(context.Context, access.Authority, limits.DebitIntent, int, []limits.DebitOwner) (limits.DebitState, error)
}

type WorkflowTool = mcp.WorkflowTool

type WorkflowApprovalStore interface {
	Pause(context.Context, access.Authority, agents.WorkflowApprovalRequest, int) (string, error)
	Continue(context.Context, access.Authority, string, json.RawMessage, map[string]json.RawMessage, int) (agents.WorkflowContinuation, error)
}

type WorkflowToolRunner interface {
	Resolve(context.Context, access.Authority, *runtime.Release, []string) ([]WorkflowTool, error)
	Call(context.Context, access.Authority, *runtime.Release, WorkflowTool, string, int, string, json.RawMessage) (json.RawMessage, error)
}

type WorkflowApprovedToolRunner interface {
	CallApproved(context.Context, access.Authority, *runtime.Release, WorkflowTool, string, int, string, json.RawMessage, agents.WorkflowApproval) (json.RawMessage, error)
}

type workflowCall struct {
	runID         string
	token         string
	tools         []WorkflowTool
	ordinal       int
	calls         int
	finalFrames   []byte
	responseItems []responseMCPItem
	responseMCP   bool
	transcript    json.RawMessage
	resume        *workflowResumeState
	approvals     []agents.WorkflowApproval
}

func (s *Server) prepareWorkflow(ctx context.Context, x *call, token string) *Error {
	if x.family != wire.Chat && x.family != wire.Responses && x.family != wire.Anthropic && x.family.Surface() != "gemini" {
		return invalidRequest("tool_execution_unsupported", "Gateway tool execution is unavailable on this surface.", nil)
	}
	if raw := x.generation.parsed.Field("n"); x.family == wire.Chat && len(raw) != 0 {
		var choices int
		if json.Unmarshal(raw, &choices) != nil || choices != 1 {
			return invalidRequest("tool_execution_choices_unsupported", "Gateway tool execution requires a single choice.", nil)
		}
	}
	if x.family.Surface() == "gemini" {
		for _, field := range []string{"generationConfig", "generation_config"} {
			raw := x.generation.parsed.Field(field)
			if len(raw) == 0 {
				continue
			}
			var config struct {
				CandidateCount      *int `json:"candidateCount"`
				CandidateCountSnake *int `json:"candidate_count"`
			}
			if json.Unmarshal(raw, &config) != nil || config.CandidateCount != nil && *config.CandidateCount != 1 || config.CandidateCountSnake != nil && *config.CandidateCountSnake != 1 {
				return invalidRequest("tool_execution_choices_unsupported", "Gateway tool execution requires a single choice.", nil)
			}
		}
	}
	tools, err := s.WorkflowTools.Resolve(ctx, x.authority, x.request.release, x.generation.toolsets)
	if err != nil {
		return workflowError(err)
	}
	if len(tools) == 0 {
		for _, field := range []string{"tools", "tool_choice", "parallel_tool_calls", "toolConfig"} {
			if len(x.generation.parsed.Field(field)) == 0 {
				continue
			}
			parsed, err := x.generation.parsed.WithResourceBindings(oif.Change{Pointer: oif.Pointer("", field), Remove: true, Reason: "empty gateway toolset selection"})
			if err != nil {
				return invalidRequest("invalid_request", "The route tools could not be removed from the request.", nil)
			}
			x.generation.parsed = parsed
		}
		x.workflow = &workflowCall{token: token, tools: tools, transcript: x.generation.parsed.Field(workflowTranscriptField(x.family))}
		return nil
	}
	raw, err := workflowDefinitions(x.family, tools)
	if err != nil {
		return serverError(http.StatusServiceUnavailable, "tool_execution_unavailable", "The route tools could not be prepared.")
	}
	parsed, err := x.generation.parsed.WithField("tools", raw)
	if err != nil {
		return invalidRequest("invalid_request", "The route tools could not be added to the request.", nil)
	}
	x.generation.parsed = parsed
	x.workflow = &workflowCall{token: token, tools: tools, transcript: x.generation.parsed.Field(workflowTranscriptField(x.family)), responseMCP: x.family == wire.Responses && x.route.ResponsesMCPItems}
	if x.workflow.responseMCP {
		x.workflow.responseItems = responseMCPListings(x.generation.toolsets, tools)
	}
	return nil
}

func workflowDefinitions(family wire.Format, tools []WorkflowTool) (json.RawMessage, error) {
	if family == wire.Responses {
		return json.Marshal(slices.Collect(func(yield func(responseAdvertisedTool) bool) {
			for _, tool := range tools {
				if !yield(responseAdvertisedTool{Type: "function", Name: tool.Definition.Alias, Description: tool.Definition.Description, Parameters: tool.Definition.InputSchema}) {
					return
				}
			}
		}))
	}
	if family == wire.Chat {
		advertised := make([]advertisedTool, len(tools))
		for i, tool := range tools {
			advertised[i] = advertisedTool{Type: "function", Function: advertisedFunction{Name: tool.Definition.Alias, Description: tool.Definition.Description, Parameters: tool.Definition.InputSchema}}
		}
		return json.Marshal(advertised)
	}
	definitions := make([]protocols.Tool, len(tools))
	for i, tool := range tools {
		definitions[i] = protocols.Tool{Name: tool.Definition.Alias, Description: tool.Definition.Description, Schema: tool.Definition.InputSchema}
	}
	return protocols.WorkflowDefinitions(family, definitions)
}

func (s *Server) checkWorkflowAuthority(ctx context.Context, x *call, route *routing.Route, toolsetID string) *Error {
	authority, err := s.reauthenticateSession(ctx, x, x.workflow.token)
	if errors.Is(err, runtime.ErrStaleAuthority) {
		return serverError(http.StatusServiceUnavailable, "authority_unavailable", "Key authority is unavailable; retry shortly.")
	}
	if err != nil || authority == nil || authority.ID != x.authority.ID || authority.DelegationAgentID != x.authority.DelegationAgentID || authority.DelegationMemberID != x.authority.DelegationMemberID || authority.DelegationPairDigest != x.authority.DelegationPairDigest {
		return authenticationError("invalid_api_key", "This API key is no longer authorized.")
	}
	if e := s.keyAuthorizer(*authority)(x.named()); e != nil {
		return e
	}
	if e := s.keyAuthorizer(*authority)(route); e != nil {
		return e
	}
	if !authority.AllowsEndUser(x.endUserDigest) || (authority.WorkloadIssuerID != nil && authority.EndUserDigest != x.endUserDigest) || !authority.AllowsAttribution(x.attribution) || !authority.AllowsClientIP(x.request.clientIP) {
		return permissionError("permission_denied", "This API key no longer permits the request.")
	}
	if toolsetID != "" && (authority.ProjectID == nil || !authority.AllowsResource("tools", toolsetID, *authority.ProjectID, s.now())) {
		return permissionError("permission_denied", "This API key no longer permits the toolset.")
	}
	return nil
}

func (s *Server) monitorWorkflowAuthority(ctx context.Context, x *call, toolsetID string) (context.Context, func() *Error) {
	callCtx, cancel := context.WithCancel(ctx)
	result := make(chan *Error, 1)
	done := make(chan struct{})
	route := x.route
	go func() {
		defer close(done)
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-callCtx.Done():
				return
			case <-ticker.C:
				checkCtx, stopCheck := context.WithTimeout(callCtx, time.Second)
				e := s.checkWorkflowAuthority(checkCtx, x, route, toolsetID)
				stopCheck()
				if e != nil {
					result <- e
					cancel()
					return
				}
			}
		}
	}()
	return callCtx, func() *Error {
		cancel()
		<-done
		select {
		case e := <-result:
			return e
		default:
			return nil
		}
	}
}

func workflowError(err error) *Error {
	var failure *mcp.WorkflowError
	if errors.As(err, &failure) {
		return &Error{Status: failure.Status, Type: "invalid_request_error", Code: failure.Code, Message: failure.Message}
	}
	return serverError(http.StatusServiceUnavailable, "tool_execution_unavailable", "The workflow tool service is unavailable.")
}

func (s *Server) abandonWorkflowDebit(ctx context.Context, x *call) *Error {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err := func() error {
		if !x.sessionDebit.dispatched {
			return s.Debits.ReleaseUndispatched(releaseCtx, x.sessionDebit.id, x.sessionDebit.claim)
		}
		ledger, ok := s.Debits.(interface {
			MarkNotSent(context.Context, string, string) error
		})
		if !ok {
			return errors.New("workflow ledger cannot release a proven unsent dispatch")
		}
		return ledger.MarkNotSent(releaseCtx, x.sessionDebit.id, x.sessionDebit.claim)
	}()
	if err != nil {
		if s.log != nil {
			s.log.Warn("workflow debit release failed", "error", err.Error())
		}
		s.markSessionUnknown(ctx, x)
		return limitsUnavailable()
	}
	x.facts[len(x.facts)-1].Execution = nil
	x.sessionDebit = nil
	return nil
}

func (s *Server) executeToolLoop(ctx context.Context, x *call) *outcome {
	firstIteration := 0
	if x.workflow.resume != nil {
		state := x.workflow.resume
		x.workflow.ordinal, x.workflow.calls = state.Ordinal, state.Calls
		parsed, err := x.generation.parsed.WithField(workflowTranscriptField(x.family), state.Transcript)
		if err != nil {
			return &outcome{err: invalidRequest("tool_approval_invalid", "The approved workflow could not be resumed.", nil)}
		}
		x.generation.parsed = parsed
		continued, failure := s.continueToolLoop(ctx, x, state.Completion)
		if failure != nil {
			return &outcome{err: failure}
		}
		x.generation.parsed = continued
		firstIteration = state.Iteration + 1
	}
	for iteration := firstIteration; iteration < 8; iteration++ {
		if e := s.checkWorkflowAuthority(ctx, x, x.route, ""); e != nil {
			return &outcome{err: e}
		}
		var frames bytes.Buffer
		if x.generation.parsed.Stream {
			x.generation.emit = func(frame []byte) error {
				if int64(frames.Len()+len(frame)) > s.cfg.MaxResponseBytes {
					return errResponseTooLarge
				}
				_, err := frames.Write(frame)
				return err
			}
		}
		out := s.executeModelStep(ctx, x, x.generation.parsed)
		if out.err == nil && out.completion != nil && x.workflow.responseMCP && !x.responseMCPContractCertified() {
			x.workflow.responseMCP = false
			x.workflow.responseItems = nil
		}
		if out.err == nil && out.completion != nil && x.generation.parsed.Stream {
			completion, err := workflowStreamCompletion(x.family, bytes.NewReader(frames.Bytes()), int(s.cfg.MaxEventBytes), int(s.cfg.MaxResponseBytes), x.named().Slug)
			if err != nil {
				return &outcome{err: serverError(http.StatusBadGateway, "invalid_tool_response", "The model returned an invalid streaming response.")}
			}
			out.completion.ToolCalls, out.completion.OutputText, out.completion.Refusal = completion.ToolCalls, completion.OutputText, completion.Refusal
			if len(completion.ToolCalls) != 0 && x.family == wire.Responses {
				out.completion.Body = completion.Body
			} else if len(completion.ToolCalls) != 0 && x.family != wire.Chat {
				body, err := protocols.WorkflowStreamBody(x.family, bytes.NewReader(frames.Bytes()), int(s.cfg.MaxEventBytes))
				if err != nil {
					return &outcome{err: serverError(http.StatusBadGateway, "invalid_tool_response", "The model returned an invalid streaming response.")}
				}
				out.completion.Body = body
			}
		}
		if out.err == nil && out.completion != nil && !x.generation.parsed.Stream && x.family.Surface() == "gemini" && !protocols.WorkflowSingleCandidate(out.completion.Body) {
			return &outcome{err: serverError(http.StatusBadGateway, "invalid_tool_response", "The model returned multiple candidates.")}
		}
		if out.err != nil || out.completion == nil || len(out.completion.ToolCalls) == 0 {
			if out.err == nil && out.completion != nil && !x.generation.parsed.Stream && len(x.workflow.responseItems) > 0 {
				body, err := projectResponseMCPItems(out.completion.Body, x.workflow.responseItems, s.cfg.MaxResponseBytes)
				if err != nil {
					return &outcome{err: serverError(http.StatusBadGateway, "invalid_tool_response", "The workflow response could not be prepared.")}
				}
				out.completion.Body = body
			}
			if out.err == nil && x.generation.parsed.Stream {
				if len(x.workflow.responseItems) > 0 {
					projected, err := projectResponseMCPFrames(frames.Bytes(), x.workflow.responseItems, int(s.cfg.MaxEventBytes), s.cfg.MaxResponseBytes)
					if err != nil {
						return &outcome{err: serverError(http.StatusBadGateway, "invalid_tool_response", "The workflow response could not be prepared.")}
					}
					x.workflow.finalFrames = projected
				} else {
					x.workflow.finalFrames = slices.Clone(frames.Bytes())
				}
			}
			return out
		}
		if iteration == 7 {
			return &outcome{err: invalidRequest("tool_iteration_limit", "The workflow reached its model iteration limit.", nil)}
		}
		if len(out.completion.ToolCalls) > 32-x.workflow.calls {
			return &outcome{err: invalidRequest("tool_call_limit", "The workflow reached its tool call limit.", nil)}
		}
		pause, failure := s.pauseWorkflow(ctx, x, out.completion, iteration)
		if failure != nil {
			return &outcome{err: failure}
		}
		if pause != nil {
			return &outcome{completion: out.completion, approval: pause}
		}
		parsed, failure := s.continueToolLoop(ctx, x, out.completion)
		if failure != nil {
			return &outcome{err: failure}
		}
		x.generation.parsed = parsed
	}
	return &outcome{err: invalidRequest("tool_iteration_limit", "The workflow reached its model iteration limit.", nil)}
}

func workflowTranscriptField(family wire.Format) string {
	if family.Surface() == "gemini" {
		return "contents"
	}
	if family == wire.Responses {
		return "input"
	}
	return "messages"
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
		return openai.Stream(wire.Chat, frames, maxEvent, route, true, func([]byte) error { return nil })
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
	return openai.Stream(wire.Chat, &translated, maxEvent, route, true, func([]byte) error { return nil })
}

func (s *Server) continueToolLoop(ctx context.Context, x *call, completion *wire.Completion) (*wire.Request, *Error) {
	var response struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	var messages []json.RawMessage
	if x.family == wire.Chat && x.generation.parsed.Stream {
		calls := make([]map[string]any, len(completion.ToolCalls))
		for i, call := range completion.ToolCalls {
			calls[i] = map[string]any{"id": call.ID, "type": "function", "function": map[string]string{"name": call.Name, "arguments": call.Arguments}}
		}
		assistant, err := json.Marshal(map[string]any{"role": "assistant", "content": completion.OutputText, "tool_calls": calls})
		if err != nil {
			return nil, serverError(http.StatusBadGateway, "invalid_tool_response", "The model returned an invalid tool response.")
		}
		response.Choices = append(response.Choices, struct {
			Message json.RawMessage `json:"message"`
		}{Message: assistant})
	} else if x.family == wire.Chat && (json.Unmarshal(completion.Body, &response) != nil || len(response.Choices) != 1 || len(response.Choices[0].Message) == 0) {
		return nil, serverError(http.StatusBadGateway, "invalid_tool_response", "The model returned an invalid tool response.")
	}
	field := "messages"
	if x.family.Surface() == "gemini" {
		field = "contents"
	} else if x.family == wire.Responses {
		field = "input"
	}
	input := x.generation.parsed.Field(field)
	if x.family == wire.Responses && len(input) > 0 && input[0] == '"' {
		message, err := json.Marshal(struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}{Role: "user", Content: input})
		if err != nil {
			return nil, invalidRequest("invalid_request", "The request messages could not be continued.", nil)
		}
		messages = []json.RawMessage{message}
	} else if err := json.Unmarshal(input, &messages); err != nil {
		return nil, invalidRequest("invalid_request", "The request messages could not be continued.", nil)
	}
	if x.family == wire.Chat {
		messages = append(messages, response.Choices[0].Message)
	}
	results := make([]protocols.WorkflowResult, 0, len(completion.ToolCalls))
	for _, call := range completion.ToolCalls {
		if e := s.checkWorkflowAuthority(ctx, x, x.route, ""); e != nil {
			return nil, e
		}
		index := slices.IndexFunc(x.workflow.tools, func(tool WorkflowTool) bool { return tool.Definition.Alias == call.Name })
		if index < 0 || call.ID == "" || !json.Valid([]byte(call.Arguments)) {
			return nil, invalidRequest("invalid_tool_call", "The model requested an unavailable tool or invalid arguments.", nil)
		}
		callCtx, finish := s.monitorWorkflowAuthority(ctx, x, x.workflow.tools[index].ToolsetID)
		result, err := s.callWorkflowTool(callCtx, x, x.workflow.tools[index], call)
		authorityErr := finish()
		x.workflow.ordinal++
		x.workflow.calls++
		if authorityErr != nil {
			return nil, authorityErr
		}
		if err != nil {
			return nil, workflowError(err)
		}
		if x.workflow.responseMCP {
			item, err := responseMCPCall(x.workflow.tools[index], call, result)
			if err != nil {
				return nil, serverError(http.StatusBadGateway, "invalid_tool_response", "The tool returned an invalid response.")
			}
			x.workflow.responseItems = append(x.workflow.responseItems, item)
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
			return nil, serverError(http.StatusServiceUnavailable, "tool_execution_unavailable", "The tool result could not be continued.")
		}
		messages = append(messages, message)
	}
	if x.family != wire.Chat {
		turns, err := protocols.WorkflowTurns(x.family, completion, results)
		if err != nil {
			return nil, serverError(http.StatusBadGateway, "invalid_tool_response", "The model returned an invalid tool response.")
		}
		messages = append(messages, turns...)
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		return nil, serverError(http.StatusServiceUnavailable, "tool_execution_unavailable", "The workflow could not be continued.")
	}
	parsed, err := x.generation.parsed.WithField(field, raw)
	if err != nil {
		return nil, invalidRequest("invalid_request", "The request messages could not be continued.", nil)
	}
	return parsed, nil
}

func selectToolsets(allowed []string, request *wire.Request) ([]string, *wire.Request, *Error) {
	raw := request.Field("olp_toolsets")
	if len(raw) == 0 {
		return slices.Clone(allowed), request, nil
	}
	var selected []string
	if err := json.Unmarshal(raw, &selected); err != nil || selected == nil {
		return nil, nil, invalidRequest("invalid_toolsets", "Use an array of toolset IDs.", nil)
	}
	for index, id := range selected {
		if !slices.Contains(allowed, id) || slices.Index(selected, id) != index {
			return nil, nil, invalidRequest("toolset_not_permitted", "The request can only select distinct toolsets allowed by the route.", nil)
		}
	}
	stripped, err := request.WithResourceBindings(oif.Change{Pointer: oif.Pointer("", "olp_toolsets"), Remove: true, Reason: "gateway toolset selection"})
	if err != nil {
		return nil, nil, invalidRequest("invalid_toolsets", "The request toolsets could not be processed.", nil)
	}
	return selected, stripped, nil
}
