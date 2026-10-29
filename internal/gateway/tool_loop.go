package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/oif"
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

type WorkflowToolRunner interface {
	Resolve(context.Context, access.Authority, *runtime.Release, []string) ([]WorkflowTool, error)
	Call(context.Context, access.Authority, *runtime.Release, WorkflowTool, string, int, string, json.RawMessage) (json.RawMessage, error)
}

type workflowCall struct {
	runID       string
	token       string
	tools       []WorkflowTool
	ordinal     int
	calls       int
	finalFrames []byte
}

type advertisedTool struct {
	Type     string             `json:"type"`
	Function advertisedFunction `json:"function"`
}

type advertisedFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

func (s *Server) prepareWorkflow(ctx context.Context, x *call, token string) *Error {
	if x.family != wire.Chat {
		return invalidRequest("tool_execution_unsupported", "Gateway tool execution is available on Chat Completions.", nil)
	}
	if raw := x.generation.parsed.Field("n"); len(raw) != 0 {
		var choices int
		if json.Unmarshal(raw, &choices) != nil || choices != 1 {
			return invalidRequest("tool_execution_choices_unsupported", "Gateway tool execution requires a single choice.", nil)
		}
	}
	tools, err := s.WorkflowTools.Resolve(ctx, x.authority, x.request.release, x.generation.toolsets)
	if err != nil {
		return workflowError(err)
	}
	if len(tools) == 0 {
		for _, field := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
			if len(x.generation.parsed.Field(field)) == 0 {
				continue
			}
			parsed, err := x.generation.parsed.WithResourceBindings(oif.Change{Pointer: oif.Pointer("", field), Remove: true, Reason: "empty gateway toolset selection"})
			if err != nil {
				return invalidRequest("invalid_request", "The route tools could not be removed from the request.", nil)
			}
			x.generation.parsed = parsed
		}
		x.workflow = &workflowCall{token: token, tools: tools}
		return nil
	}
	advertised := make([]advertisedTool, len(tools))
	for i, tool := range tools {
		advertised[i] = advertisedTool{Type: "function", Function: advertisedFunction{Name: tool.Definition.Alias, Description: tool.Definition.Description, Parameters: tool.Definition.InputSchema}}
	}
	raw, err := json.Marshal(advertised)
	if err != nil {
		return serverError(http.StatusServiceUnavailable, "tool_execution_unavailable", "The route tools could not be prepared.")
	}
	parsed, err := x.generation.parsed.WithField("tools", raw)
	if err != nil {
		return invalidRequest("invalid_request", "The route tools could not be added to the request.", nil)
	}
	x.generation.parsed = parsed
	x.workflow = &workflowCall{token: token, tools: tools}
	return nil
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
	for iteration := 0; iteration < 8; iteration++ {
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
		if out.err == nil && out.completion != nil && x.generation.parsed.Stream {
			completion, err := openai.Stream(wire.Chat, bytes.NewReader(frames.Bytes()), int(s.cfg.MaxEventBytes), x.named().Slug, true, func([]byte) error { return nil })
			if err != nil {
				return &outcome{err: serverError(http.StatusBadGateway, "invalid_tool_response", "The model returned an invalid streaming response.")}
			}
			out.completion.ToolCalls, out.completion.OutputText, out.completion.Refusal = completion.ToolCalls, completion.OutputText, completion.Refusal
		}
		if out.err != nil || out.completion == nil || len(out.completion.ToolCalls) == 0 {
			if out.err == nil && x.generation.parsed.Stream {
				x.workflow.finalFrames = slices.Clone(frames.Bytes())
			}
			return out
		}
		if iteration == 7 {
			return &outcome{err: invalidRequest("tool_iteration_limit", "The workflow reached its model iteration limit.", nil)}
		}
		if len(out.completion.ToolCalls) > 32-x.workflow.calls {
			return &outcome{err: invalidRequest("tool_call_limit", "The workflow reached its tool call limit.", nil)}
		}
		parsed, failure := s.continueToolLoop(ctx, x, out.completion)
		if failure != nil {
			return &outcome{err: failure}
		}
		x.generation.parsed = parsed
	}
	return &outcome{err: invalidRequest("tool_iteration_limit", "The workflow reached its model iteration limit.", nil)}
}

func (s *Server) continueToolLoop(ctx context.Context, x *call, completion *wire.Completion) (*wire.Request, *Error) {
	var response struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	var messages []json.RawMessage
	if x.generation.parsed.Stream {
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
	} else if err := json.Unmarshal(completion.Body, &response); err != nil || len(response.Choices) != 1 || len(response.Choices[0].Message) == 0 {
		return nil, serverError(http.StatusBadGateway, "invalid_tool_response", "The model returned an invalid tool response.")
	}
	if err := json.Unmarshal(x.generation.parsed.Field("messages"), &messages); err != nil {
		return nil, invalidRequest("invalid_request", "The request messages could not be continued.", nil)
	}
	messages = append(messages, response.Choices[0].Message)
	for _, call := range completion.ToolCalls {
		if e := s.checkWorkflowAuthority(ctx, x, x.route, ""); e != nil {
			return nil, e
		}
		index := slices.IndexFunc(x.workflow.tools, func(tool WorkflowTool) bool { return tool.Definition.Alias == call.Name })
		if index < 0 || call.ID == "" || !json.Valid([]byte(call.Arguments)) {
			return nil, invalidRequest("invalid_tool_call", "The model requested an unavailable tool or invalid arguments.", nil)
		}
		callCtx, finish := s.monitorWorkflowAuthority(ctx, x, x.workflow.tools[index].ToolsetID)
		result, err := s.WorkflowTools.Call(callCtx, x.authority, x.request.release, x.workflow.tools[index], x.workflow.runID, x.workflow.ordinal, fmt.Sprintf("%s:%d:%s", x.workflow.runID, x.workflow.ordinal, call.ID), json.RawMessage(call.Arguments))
		authorityErr := finish()
		x.workflow.ordinal++
		x.workflow.calls++
		if authorityErr != nil {
			return nil, authorityErr
		}
		if err != nil {
			return nil, workflowError(err)
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
	raw, err := json.Marshal(messages)
	if err != nil {
		return nil, serverError(http.StatusServiceUnavailable, "tool_execution_unavailable", "The workflow could not be continued.")
	}
	parsed, err := x.generation.parsed.WithField("messages", raw)
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
