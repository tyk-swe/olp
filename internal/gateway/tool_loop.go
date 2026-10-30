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
	"github.com/tyk-swe/olp/internal/usage"
)

type WorkflowRunStore interface {
	Start(context.Context, access.Authority, string, string) (string, error)
	Prepare(context.Context, access.Authority, limits.DebitIntent, int, []limits.DebitOwner) (limits.DebitState, error)
}

type WorkflowTool = mcp.WorkflowTool

const recoveryToolName = "olp_recover_context"

type WorkflowApprovalStore interface {
	Pause(context.Context, access.Authority, agents.WorkflowApprovalRequest, int) (string, error)
	Continue(context.Context, access.Authority, string, json.RawMessage, map[string]json.RawMessage, int) (agents.WorkflowContinuation, error)
}

type WorkflowToolRunner interface {
	Resolve(context.Context, access.Authority, *runtime.Release, []string) ([]WorkflowTool, error)
	AuthorizeWorkflowTool(access.Authority, WorkflowTool) error
	Call(context.Context, access.Authority, *runtime.Release, WorkflowTool, string, int, string, json.RawMessage) (json.RawMessage, error)
}

type WorkflowApprovedToolRunner interface {
	CallApproved(context.Context, access.Authority, *runtime.Release, WorkflowTool, string, int, string, json.RawMessage, agents.WorkflowApproval) (json.RawMessage, error)
}

type workflowCall struct {
	toolCost      usage.Cost
	priorCost     usage.Cost
	unknownCost   bool
	startedAt     time.Time
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
	resumeStart   int
	approvals     []agents.WorkflowApproval
}

func workflowBounds(route, key *limits.WorkflowBounds, request *wire.Request) (limits.WorkflowBounds, limits.WorkflowBounds, *wire.Request, *Error) {
	raw := request.Field("olp_run_limits")
	caller, ok := limits.ParseWorkflowBounds(raw)
	if !ok {
		return limits.WorkflowBounds{}, limits.WorkflowBounds{}, nil, invalidRequest("invalid_run_limits", "Use valid workflow run limits.", nil)
	}
	resolved := limits.ResolveWorkflowBounds(nil, route, key, &caller)
	if len(raw) == 0 {
		return resolved, caller, request, nil
	}
	stripped, err := request.WithResourceBindings(oif.Change{Pointer: oif.Pointer("", "olp_run_limits"), Remove: true, Reason: "gateway workflow bounds"})
	if err != nil {
		return limits.WorkflowBounds{}, limits.WorkflowBounds{}, nil, invalidRequest("invalid_run_limits", "The workflow limits could not be processed.", nil)
	}
	return resolved, caller, stripped, nil
}

func (s *Server) prepareWorkflow(ctx context.Context, x *call, token string) *Error {
	switch refusal := mcp.WorkflowRequestRefusal(x.generation.parsed); refusal {
	case "tool_execution_unsupported":
		return invalidRequest("tool_execution_unsupported", "Gateway tool execution is unavailable on this surface.", nil)
	case "tool_execution_choices_unsupported":
		return invalidRequest(refusal, "Gateway tool execution requires a single choice.", nil)
	}
	tools, err := s.WorkflowTools.Resolve(ctx, x.authority, x.request.release, x.generation.toolsets)
	if err != nil {
		return workflowError(err)
	}
	if x.guardrailEngine.HasCompression() {
		zero := "0"
		tools = append(tools, WorkflowTool{Definition: routing.MCPToolsetTool{
			Alias: recoveryToolName, Name: recoveryToolName, Classification: "read_only", UnitPrice: &zero,
			Description: "Recover the original content of a compressed input using the recovery id shown beside it.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer","minimum":0}},"required":["id"],"additionalProperties":false}`),
		}})
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
		x.workflow = &workflowCall{token: token, tools: tools, transcript: x.generation.parsed.Field(x.family.WorkflowTranscriptField())}
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
	x.workflow = &workflowCall{token: token, tools: tools, transcript: x.generation.parsed.Field(x.family.WorkflowTranscriptField()), responseMCP: x.family == wire.Responses && x.named().ResponsesMCPItems}
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

func (s *Server) workflowAuthority(ctx context.Context, x *call, route *routing.Route, toolsetID string) (*access.Authority, *Error) {
	authority, err := s.reauthenticateSession(ctx, x, x.workflow.token)
	if errors.Is(err, runtime.ErrStaleAuthority) {
		return nil, serverError(http.StatusServiceUnavailable, "authority_unavailable", "Key authority is unavailable; retry shortly.")
	}
	if err != nil || authority == nil || authority.ID != x.authority.ID || authority.DelegationAgentID != x.authority.DelegationAgentID || authority.DelegationMemberID != x.authority.DelegationMemberID || authority.DelegationPairDigest != x.authority.DelegationPairDigest {
		return nil, authenticationError("invalid_api_key", "This API key is no longer authorized.")
	}
	if e := s.keyAuthorizer(*authority)(x.named()); e != nil {
		return nil, e
	}
	if e := s.keyAuthorizer(*authority)(route); e != nil {
		return nil, e
	}
	if !authority.AllowsEndUser(x.endUserDigest) || (authority.WorkloadIssuerID != nil && authority.EndUserDigest != x.endUserDigest) || !authority.AllowsAttribution(x.attribution) || !authority.AllowsClientIP(x.request.clientIP) {
		return nil, permissionError("permission_denied", "This API key no longer permits the request.")
	}
	if toolsetID != "" && (authority.ProjectID == nil || !authority.AllowsResource("tools", toolsetID, *authority.ProjectID, s.now())) {
		return nil, permissionError("permission_denied", "This API key no longer permits the toolset.")
	}
	for _, tool := range x.workflow.tools {
		if tool.ToolsetID == "" && tool.Definition.Alias == recoveryToolName {
			continue
		}
		if err := s.WorkflowTools.AuthorizeWorkflowTool(*authority, tool); err != nil {
			return nil, workflowError(err)
		}
	}
	return authority, nil
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
				_, e := s.workflowAuthority(checkCtx, x, route, toolsetID)
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
	firstIteration, resumeError := func() (int, *Error) {
		if x.workflow.resume == nil {
			return 0, nil
		}
		state := x.workflow.resume
		x.workflow.calls = state.Calls
		parsed, err := x.generation.parsed.WithField(x.family.WorkflowTranscriptField(), state.Transcript)
		if err != nil {
			return 0, invalidRequest("tool_approval_invalid", "The approved workflow could not be resumed.", nil)
		}
		x.generation.parsed = parsed
		if x.guardrailEngine != nil && x.guardrailEngine.HasCompression() {
			x.guardrailCtx = context.WithValue(ctx, guardrailCallerKey{}, x)
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
	}()
	if resumeError != nil {
		return &outcome{err: resumeError}
	}
	for iteration := firstIteration; int64(iteration) < *x.generation.runLimits.MaxIterations; iteration++ {
		if iteration > 0 {
			parsed, failure := s.pruneWorkflow(ctx, x, x.generation.parsed)
			if failure != nil {
				return &outcome{err: failure}
			}
			x.generation.parsed = parsed
		}
		authority, e := s.workflowAuthority(ctx, x, x.route, "")
		if e != nil {
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
		out := s.executeModelStep(ctx, x, x.generation.parsed, *authority)
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
		if int64(iteration)+1 == *x.generation.runLimits.MaxIterations {
			return &outcome{err: invalidRequest("tool_iteration_limit", "The workflow reached its model iteration limit.", nil)}
		}
		if int64(len(out.completion.ToolCalls)+x.workflow.calls) > *x.generation.runLimits.MaxToolCalls {
			return &outcome{err: invalidRequest("tool_call_limit", "The workflow reached its tool call limit.", nil)}
		}
		pause, failure := s.pauseWorkflow(ctx, x, out.completion, iteration)
		if failure != nil {
			return &outcome{err: failure}
		}
		if pause != nil {
			return &outcome{completion: out.completion, approval: pause}
		}
		parsed, failure := s.continueToolLoop(ctx, x, out.completion, x.workflow.ordinal)
		if failure != nil {
			return &outcome{err: failure}
		}
		x.generation.parsed = parsed
	}
	return &outcome{err: invalidRequest("tool_iteration_limit", "The workflow reached its model iteration limit.", nil)}
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

func (s *Server) continueToolLoop(ctx context.Context, x *call, completion *wire.Completion, identityStart int) (*wire.Request, *Error) {
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
	field := x.family.WorkflowTranscriptField()
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
	for callIndex, call := range completion.ToolCalls {
		authority, e := s.workflowAuthority(ctx, x, x.route, "")
		if e != nil {
			return nil, e
		}
		index := slices.IndexFunc(x.workflow.tools, func(tool WorkflowTool) bool { return tool.Definition.Alias == call.Name })
		if index < 0 || call.ID == "" || slices.Contains(x.workflowRecoveryCalls, call.ID) || !json.Valid([]byte(call.Arguments)) {
			return nil, invalidRequest("invalid_tool_call", "The model requested an unavailable tool or invalid arguments.", nil)
		}
		// A shallow copy retains fresh limits while binding request-local attribution.
		toolAuthority := *authority
		toolAuthority.Attribution = x.attribution
		toolCost, costKnown, costErr := x.workflowToolCost(x.workflow.tools[index])
		if costErr != nil {
			return nil, costErr
		}
		callCtx, finish := s.monitorWorkflowAuthority(ctx, x, x.workflow.tools[index].ToolsetID)
		result, err := s.callWorkflowTool(callCtx, x, toolAuthority, x.workflow.tools[index], call, identityStart+callIndex)
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
				return nil, serverError(http.StatusBadGateway, "invalid_tool_response", "The tool returned an invalid response.")
			}
			x.workflow.responseItems = append(x.workflow.responseItems, item)
		}
		x.workflow.toolCost = x.workflow.toolCost.Add(toolCost)
		x.workflow.unknownCost = x.workflow.unknownCost || !costKnown
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
	selected, err := mcp.SelectWorkflowToolsets(allowed, raw)
	if errors.Is(err, mcp.ErrWorkflowToolsetsInvalid) {
		return nil, nil, invalidRequest("invalid_toolsets", "Use an array of toolset IDs.", nil)
	}
	if err != nil {
		return nil, nil, invalidRequest("toolset_not_permitted", "The request can only select distinct toolsets allowed by the route.", nil)
	}
	if len(raw) == 0 {
		return selected, request, nil
	}
	stripped, err := request.WithResourceBindings(oif.Change{Pointer: oif.Pointer("", "olp_toolsets"), Remove: true, Reason: "gateway toolset selection"})
	if err != nil {
		return nil, nil, invalidRequest("invalid_toolsets", "The request toolsets could not be processed.", nil)
	}
	return selected, stripped, nil
}
