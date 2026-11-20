package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/agents/runs"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

type WorkflowRunStore interface {
	Start(context.Context, keyauthority.Authority, string, string) (string, error)
	Prepare(context.Context, keyauthority.Authority, debits.Intent, int, []limits.Owner) (debits.State, error)
	Pause(context.Context, keyauthority.Authority, runs.WorkflowApprovalRequest, int) (string, error)
	Continue(context.Context, keyauthority.Authority, string, json.RawMessage, map[string]json.RawMessage, int) (runs.WorkflowContinuation, error)
}

type WorkflowTool = mcp.WorkflowTool

const recoveryToolName = "olp_recover_context"

type WorkflowToolRunner interface {
	Resolve(context.Context, keyauthority.Authority, *release.Release, []string) ([]WorkflowTool, error)
	AuthorizeWorkflowTool(keyauthority.Authority, WorkflowTool) error
	Call(context.Context, keyauthority.Authority, *release.Release, WorkflowTool, string, int, string, json.RawMessage) (json.RawMessage, error)
	CallApproved(context.Context, keyauthority.Authority, *release.Release, WorkflowTool, string, int, string, json.RawMessage, runs.WorkflowApproval) (json.RawMessage, error)
}

type workflowCall struct {
	toolCost      rates.Cost
	priorCost     rates.Cost
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
	approvals     []runs.WorkflowApproval
}

func workflowBounds(route, key *limits.WorkflowBounds, request *wire.Request) (limits.WorkflowBounds, limits.WorkflowBounds, *wire.Request, *fault.Error) {
	raw := request.Field("olp_run_limits")
	caller, ok := limits.ParseWorkflowBounds(raw)
	if !ok {
		return limits.WorkflowBounds{}, limits.WorkflowBounds{}, nil, fault.InvalidRequest("invalid_run_limits", "Use valid workflow run limits.", nil)
	}
	resolved := limits.ResolveWorkflowBounds(nil, route, key, &caller)
	if len(raw) == 0 {
		return resolved, caller, request, nil
	}
	stripped, err := request.WithResourceBindings(oif.Change{Pointer: oif.Pointer("", "olp_run_limits"), Remove: true, Reason: "gateway workflow bounds"})
	if err != nil {
		return limits.WorkflowBounds{}, limits.WorkflowBounds{}, nil, fault.InvalidRequest("invalid_run_limits", "The workflow limits could not be processed.", nil)
	}
	return resolved, caller, stripped, nil
}

func (s *Server) prepareWorkflow(ctx context.Context, x *Call, token string) *fault.Error {
	switch refusal := mcp.WorkflowRequestRefusal(x.generation.parsed); refusal {
	case "tool_execution_unsupported":
		return fault.InvalidRequest("tool_execution_unsupported", "Gateway tool execution is unavailable on this surface.", nil)
	case "tool_execution_choices_unsupported":
		return fault.InvalidRequest(refusal, "Gateway tool execution requires a single choice.", nil)
	}
	tools, err := s.Deps.WorkflowTools.Resolve(ctx, x.authority, x.request.release, x.generation.toolsets)
	if err != nil {
		return workflowError(err)
	}
	if x.guard.engine.HasCompression() {
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
				return fault.InvalidRequest("invalid_request", "The route tools could not be removed from the request.", nil)
			}
			x.generation.parsed = parsed
		}
		x.workflow = &workflowCall{token: token, tools: tools, transcript: x.generation.parsed.Field(x.family.WorkflowTranscriptField())}
		return nil
	}
	raw, err := workflowDefinitions(x.family, tools)
	if err != nil {
		return fault.ServerError(http.StatusServiceUnavailable, "tool_execution_unavailable", "The route tools could not be prepared.")
	}
	parsed, err := x.generation.parsed.WithField("tools", raw)
	if err != nil {
		return fault.InvalidRequest("invalid_request", "The route tools could not be added to the request.", nil)
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

func (s *Server) workflowAuthority(ctx context.Context, x *Call, route *routing.Route, toolsetID string) (*keyauthority.Authority, *fault.Error) {
	authority, err := s.ReauthenticateSession(ctx, x, x.workflow.token)
	if errors.Is(err, release.ErrStaleAuthority) {
		return nil, fault.ServerError(http.StatusServiceUnavailable, "authority_unavailable", "Key authority is unavailable; retry shortly.")
	}
	if err != nil || authority == nil || authority.ID != x.authority.ID || authority.DelegationAgentID != x.authority.DelegationAgentID || authority.DelegationMemberID != x.authority.DelegationMemberID || authority.DelegationPairDigest != x.authority.DelegationPairDigest {
		return nil, fault.AuthenticationError("invalid_api_key", "This API key is no longer authorized.")
	}
	if e := s.keyAuthorizer(*authority)(x.named()); e != nil {
		return nil, e
	}
	if e := s.keyAuthorizer(*authority)(route); e != nil {
		return nil, e
	}
	if !authority.AllowsEndUser(x.endUserDigest) || (authority.WorkloadIssuerID != nil && authority.EndUserDigest != x.endUserDigest) || !authority.AllowsAttribution(x.attribution) || !authority.AllowsClientIP(x.request.clientIP) {
		return nil, fault.PermissionError("permission_denied", "This API key no longer permits the request.")
	}
	if toolsetID != "" && (authority.ProjectID == nil || !authority.AllowsResource("tools", toolsetID, *authority.ProjectID, s.Now())) {
		return nil, fault.PermissionError("permission_denied", "This API key no longer permits the toolset.")
	}
	for _, tool := range x.workflow.tools {
		if tool.ToolsetID == "" && tool.Definition.Alias == recoveryToolName {
			continue
		}
		if err := s.Deps.WorkflowTools.AuthorizeWorkflowTool(*authority, tool); err != nil {
			return nil, workflowError(err)
		}
	}
	return authority, nil
}

func (s *Server) monitorWorkflowAuthority(ctx context.Context, x *Call, toolsetID string) (context.Context, func() *fault.Error) {
	callCtx, cancel := context.WithCancel(ctx)
	result := make(chan *fault.Error, 1)
	done := make(chan struct{})
	route := x.Route
	go func() {
		defer close(done)
		defer panics.Recover(s.Log, "workflow authority monitor", func(error) {
			result <- fault.ServerError(http.StatusInternalServerError, "internal_error", "The workflow tool authority could not be checked.")
			cancel()
		})
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
	return callCtx, func() *fault.Error {
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

func workflowError(err error) *fault.Error {
	var failure *mcp.WorkflowError
	if errors.As(err, &failure) {
		return &fault.Error{Status: failure.Status, Type: "invalid_request_error", Code: failure.Code, Message: failure.Message}
	}
	return fault.ServerError(http.StatusServiceUnavailable, "tool_execution_unavailable", "The workflow tool service is unavailable.")
}

func (s *Server) abandonWorkflowDebit(ctx context.Context, x *Call) *fault.Error {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err := func() error {
		if !x.sessionDebit.dispatched {
			return s.Deps.Debits.ReleaseUndispatched(releaseCtx, x.sessionDebit.id, x.sessionDebit.claim)
		}
		return s.Deps.Debits.MarkNotSent(releaseCtx, x.sessionDebit.id, x.sessionDebit.claim)
	}()
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("workflow debit release failed", "error", err.Error())
		}
		s.markSessionUnknown(ctx, x)
		return quota.LimitsUnavailable()
	}
	x.facts[len(x.facts)-1].Execution = nil
	x.sessionDebit = nil
	return nil
}

func selectToolsets(allowed []string, request *wire.Request) ([]string, *wire.Request, *fault.Error) {
	raw := request.Field("olp_toolsets")
	selected, err := mcp.SelectWorkflowToolsets(allowed, raw)
	if errors.Is(err, mcp.ErrWorkflowToolsetsInvalid) {
		return nil, nil, fault.InvalidRequest("invalid_toolsets", "Use an array of toolset IDs.", nil)
	}
	if err != nil {
		return nil, nil, fault.InvalidRequest("toolset_not_permitted", "The request can only select distinct toolsets allowed by the route.", nil)
	}
	if len(raw) == 0 {
		return selected, request, nil
	}
	stripped, err := request.WithResourceBindings(oif.Change{Pointer: oif.Pointer("", "olp_toolsets"), Remove: true, Reason: "gateway toolset selection"})
	if err != nil {
		return nil, nil, fault.InvalidRequest("invalid_toolsets", "The request toolsets could not be processed.", nil)
	}
	return selected, stripped, nil
}
