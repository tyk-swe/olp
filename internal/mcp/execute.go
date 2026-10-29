package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
)

const toolRateLeaseTTL = 150 * time.Second

func toolPerKeyLookup(authority access.Authority, serverID string) string {
	sum := sha256.Sum256([]byte(authority.LimitsLookup() + "\x00" + serverID))
	return "tk_" + hex.EncodeToString(sum[:16])
}

func toolPerServerLookup(serverID string) string {
	return "ts_" + strings.ReplaceAll(serverID, "-", "")
}

func tighterToolLimit(server int64, key *int64) *int64 {
	value := server
	if key != nil && *key < value {
		value = *key
	}
	return &value
}

func (s *Server) reserveToolRate(ctx context.Context, in executeInput) ([]*limits.Lease, *apiError, time.Duration) {
	limiter := s.Limiter
	if concrete, ok := limiter.(*limits.Limiter); ok && concrete == nil {
		limiter = nil
	}
	if limiter == nil {
		return nil, fail(http.StatusServiceUnavailable, "limits_unavailable", "api_error", "Tool call limits are unavailable, so the call was not dispatched."), 0
	}
	effective := routing.EffectiveToolLimits(in.tool.CallsPerMinute, in.tool.MaxConcurrency, in.tool.PerKeyCallsPerMinute, in.tool.PerKeyMaxConcurrency)
	requests := []limits.Request{
		{
			CostOwnerID: in.authority.ID, LookupID: toolPerKeyLookup(in.authority, in.tool.ServerID),
			RequestsPerMinute: tighterToolLimit(effective.PerKeyCallsPerMinute, in.authority.Policy.RequestsPerMinute),
			MaxConcurrency:    tighterToolLimit(effective.PerKeyMaxConcurrency, in.authority.Policy.MaxConcurrency),
			LeaseTTL:          toolRateLeaseTTL,
		},
		{
			CostOwnerID:       in.tool.ServerID,
			LookupID:          toolPerServerLookup(in.tool.ServerID),
			RequestsPerMinute: &effective.CallsPerMinute,
			MaxConcurrency:    &effective.MaxConcurrency,
			LeaseTTL:          toolRateLeaseTTL,
		},
	}
	if agent := in.authority.DelegationAgentLimits; agent != nil && in.runID == "" {
		requests = append(requests, limits.Request{
			CostOwnerID:       agent.ID,
			LookupID:          agent.LookupID,
			RequestsPerMinute: agent.Limits.RequestsPerMinute,
			MaxConcurrency:    agent.Limits.MaxConcurrency,
			LeaseTTL:          toolRateLeaseTTL,
		})
	}
	leases := make([]*limits.Lease, 0, len(requests))
	for _, request := range requests {
		lease, err := limiter.Reserve(ctx, request)
		if err != nil {
			s.finishLeases(ctx, leases, false)
			var exceeded *limits.ExceededError
			if errors.As(err, &exceeded) {
				return nil, fail(http.StatusTooManyRequests, "tool_rate_limited", "rate_limit_error", "The call exceeds a tool rate or concurrency limit, so it was not dispatched."), exceeded.RetryAfter
			}
			return nil, fail(http.StatusServiceUnavailable, "limits_unavailable", "api_error", "Tool call limits are unavailable, so the call was not dispatched."), 0
		}
		if lease != nil {
			leases = append(leases, lease)
		}
	}
	return leases, nil, 0
}

type executeInput struct {
	authority access.Authority
	approval  *agents.WorkflowApproval
	runID     string
	ordinal   int
	resolved  resolvedToolset
	tool      routing.MCPToolsetTool
	arguments json.RawMessage
	releaseID string
	callID    string
	release   *runtime.Release
	snapshot  *routing.Snapshot
}

type execution struct {
	reservation    *Reservation
	result         mcpservers.ToolResult
	failure        *apiError
	blocked        *apiError
	retryAfter     time.Duration
	outcomeUnknown bool
	unknownMessage string
	listChanged    bool
	certification  certificationState
	repeat         *RetainedCall
	repeatResult   mcpservers.ToolResult
	repeatRetained bool
}

type dispatchResult struct {
	dispatched    bool
	result        mcpservers.ToolResult
	started       time.Time
	completed     time.Time
	listChanged   bool
	callErr       error
	failure       *apiError
	certification certificationState
}

func (s *Server) loadOriginalCredential(ctx context.Context, authority access.Authority, tool routing.MCPToolsetTool) ([]byte, *apiError) {
	if tool.OAuth != nil {
		if s.Grants == nil {
			return nil, fail(http.StatusForbidden, "user_grant_required", "permission_error", "Enroll a usable personal grant before calling this tool.")
		}
		credential, err := s.Grants.Resolve(ctx, authority, tool)
		if err != nil {
			return nil, fail(http.StatusForbidden, "user_grant_required", "permission_error", "Enroll a usable personal grant before calling this tool.")
		}
		return credential, nil
	}
	if tool.CredentialID == nil {
		return nil, nil
	}
	if s.Credentials == nil {
		return nil, fail(http.StatusServiceUnavailable, "credential_unavailable", "api_error", "The registered credential is unavailable, so the call was not dispatched.")
	}
	credential, err := s.Credentials(ctx, *tool.CredentialID)
	if err != nil || len(credential) == 0 {
		return nil, fail(http.StatusServiceUnavailable, "credential_unavailable", "api_error", "The registered credential is unavailable, so the call was not dispatched.")
	}
	return credential, nil
}

func (s *Server) reserveToolCall(ctx context.Context, in executeInput) (*Reservation, execution) {
	reservedResult, err := s.Ledger.Reserve(ctx, ReserveInput{
		Authority: in.authority, Approval: in.approval, RunID: in.runID, Ordinal: in.ordinal, ToolsetID: in.resolved.id, ToolsetRevisionID: in.resolved.revisionID,
		ReleaseID: in.releaseID, ToolAlias: in.tool.Alias, InputDigest: s.toolInputDigest(in.arguments),
		CallDigest: s.callDigest(in.authority, in.callID), UnitPrice: cloneUnitPrice(in.tool.UnitPrice),
		ServerID: in.tool.ServerID, ServerRevisionID: in.tool.ServerRevisionID,
	})
	if err == nil && reservedResult.Repeat != nil {
		outcome := execution{repeat: reservedResult.Repeat}
		if reservedResult.Repeat.Status == StatusSucceeded || reservedResult.Repeat.Status == StatusUpstreamError {
			outcome.repeatResult, outcome.repeatRetained = s.retainedResultFor(reservedResult.Repeat.AttemptID)
		}
		return nil, outcome
	}
	if err != nil {
		if errors.Is(err, ErrUnpricedToolCall) {
			return nil, execution{failure: fail(http.StatusUnprocessableEntity, "tool_unpriced", "invalid_request_error", "The tool has no price and an enforced budget needs a finite bound, so the call was not dispatched.")}
		}
		var exceeded *limits.ExceededError
		if errors.As(err, &exceeded) {
			return nil, execution{failure: fail(http.StatusTooManyRequests, "budget_exceeded", "rate_limit_error", "The call exceeds an enforced budget, so it was not dispatched."), retryAfter: exceeded.RetryAfter}
		}
		return nil, execution{failure: errLedger}
	}
	reservation := reservedResult.Reservation
	if reservation == nil {
		return nil, execution{failure: errLedger}
	}
	if failure := s.currentToolAuthority(in); failure != nil {
		s.release(reservation)
		return nil, execution{failure: failure}
	}
	return reservation, execution{}
}

func (s *Server) dispatchToolCall(ctx context.Context, in executeInput, credential []byte, reservation *Reservation, arguments json.RawMessage) dispatchResult {
	tool := in.tool
	transport, err := mcpservers.DialTransport(ctx, s.Egress, tool.Endpoint, credential, mcpservers.TransportOptions{ClientName: "openllmproxy-tools", Timeout: defaultCallTimeout, Transport: tool.Transport, Stdio: tool.Stdio, Executables: s.Executables})
	if err != nil {
		s.release(reservation)
		return dispatchResult{certification: certificationInvalid, failure: fail(http.StatusBadGateway, "upstream_unavailable", "api_error", "The upstream MCP server could not be reached, so the tool was not called.")}
	}
	defer transport.Close(context.WithoutCancel(ctx))
	switch state := certifyTransport(ctx, transport, tool.CatalogDigest); state {
	case certificationInvalid:
		s.release(reservation)
		return dispatchResult{certification: state, failure: fail(http.StatusBadGateway, "upstream_unavailable", "api_error", "The upstream MCP server catalog could not be verified, so the tool was not called.")}
	case certificationDrifted:
		s.release(reservation)
		return dispatchResult{certification: state, failure: errToolDefinitionChanged}
	}
	if failure := s.currentToolAuthority(in); failure != nil {
		return dispatchResult{failure: s.refuseToolIdentity(ctx, in, reservation, failure)}
	}
	if tool.OAuth != nil {
		if s.Grants == nil {
			return dispatchResult{failure: s.refuseToolIdentity(ctx, in, reservation, fail(http.StatusForbidden, "user_grant_required", "permission_error", "Enroll a usable personal grant before calling this tool."))}
		}
		if err := s.Grants.CheckGrant(ctx, in.authority, tool); err != nil {
			return dispatchResult{failure: s.refuseToolIdentity(ctx, in, reservation, fail(http.StatusForbidden, "user_grant_required", "permission_error", "Enroll a usable personal grant before calling this tool."))}
		}
	}
	started := time.Now()
	dispatchedEvidence := AttemptEvidence{
		ServerID: tool.ServerID, ServerRevisionID: tool.ServerRevisionID,
		Status: StatusDispatched, StartedAt: started, IdentityDecisions: toolIdentityDecisions(tool, nil),
	}
	if err = s.Ledger.MarkDispatched(ctx, reservation, dispatchedEvidence); err != nil {
		s.release(reservation)
		return dispatchResult{certification: certificationCertified, failure: errLedger}
	}
	result, callErr := transport.CallTool(ctx, tool.Name, arguments)
	return dispatchResult{
		dispatched: true,
		result:     result, started: started, completed: time.Now(),
		listChanged: transport.ListChanged(), callErr: callErr,
		certification: certificationCertified,
	}
}

func (s *Server) settleToolCall(ctx context.Context, in executeInput, reservation *Reservation, dispatched dispatchResult, state *toolGuardrailState) execution {
	tool := in.tool
	rawEncoded, _ := json.Marshal(dispatched.result)
	guarded, decisions, refusal := s.guardResult(ctx, in, reservation, state, dispatched.result)
	if refusal != nil {
		evidence := AttemptEvidence{
			ServerID: tool.ServerID, ServerRevisionID: tool.ServerRevisionID,
			Status: StatusGuardrailBlocked, IsError: dispatched.result.IsError, ResultDigest: digestBytes(rawEncoded),
			StartedAt: dispatched.started, CompletedAt: dispatched.completed,
			GuardrailDecisions: decisions, IdentityDecisions: toolIdentityDecisions(tool, nil),
		}
		if err := s.Ledger.Settle(context.WithoutCancel(ctx), reservation, evidence); err != nil {
			unknownEvidence := AttemptEvidence{
				ServerID: tool.ServerID, ServerRevisionID: tool.ServerRevisionID,
				Status: StatusOutcomeUnknown, IsError: dispatched.result.IsError, ResultDigest: digestBytes(rawEncoded),
				StartedAt: dispatched.started, CompletedAt: dispatched.completed, GuardrailDecisions: decisions, IdentityDecisions: toolIdentityDecisions(tool, nil),
			}
			_ = s.Ledger.MarkUnknown(context.WithoutCancel(ctx), reservation, unknownEvidence)
			return execution{reservation: reservation, outcomeUnknown: true, unknownMessage: "The tool call ran upstream, but its accounting could not be settled.", listChanged: dispatched.listChanged, certification: dispatched.certification}
		}
		return execution{reservation: reservation, blocked: guardrailRefusal(refusal), listChanged: dispatched.listChanged, certification: dispatched.certification}
	}
	encoded, _ := json.Marshal(guarded)
	evidence := AttemptEvidence{
		ServerID: tool.ServerID, ServerRevisionID: tool.ServerRevisionID,
		Status: completedToolStatus(guarded), IsError: guarded.IsError, ResultDigest: digestBytes(encoded),
		StartedAt: dispatched.started, CompletedAt: dispatched.completed,
		GuardrailDecisions: decisions, IdentityDecisions: toolIdentityDecisions(tool, nil),
	}
	if err := s.Ledger.Settle(context.WithoutCancel(ctx), reservation, evidence); err != nil {
		unknownEvidence := AttemptEvidence{
			ServerID: tool.ServerID, ServerRevisionID: tool.ServerRevisionID,
			Status: StatusOutcomeUnknown, IsError: guarded.IsError, ResultDigest: digestBytes(encoded),
			StartedAt: dispatched.started, CompletedAt: dispatched.completed,
			GuardrailDecisions: decisions, IdentityDecisions: toolIdentityDecisions(tool, nil),
		}
		_ = s.Ledger.MarkUnknown(context.WithoutCancel(ctx), reservation, unknownEvidence)
		return execution{reservation: reservation, outcomeUnknown: true, unknownMessage: "The tool call ran upstream, but its accounting could not be settled.", listChanged: dispatched.listChanged, certification: dispatched.certification}
	}
	s.retainResult(reservation.AttemptID, guarded)
	return execution{reservation: reservation, result: guarded, listChanged: dispatched.listChanged, certification: dispatched.certification}
}

func completedToolStatus(result mcpservers.ToolResult) Status {
	if result.IsError {
		return StatusUpstreamError
	}
	return StatusSucceeded
}

func (s *Server) execute(parent context.Context, in executeInput) execution {
	if failure := executeCallIDFailure(in.callID); failure != nil {
		return execution{failure: failure}
	}
	ctx, cancel := context.WithTimeout(parent, defaultCallTimeout)
	defer cancel()
	leases, rateFailure, retryAfter := s.reserveToolRate(ctx, in)
	if rateFailure != nil {
		return execution{failure: rateFailure, retryAfter: retryAfter}
	}
	dispatched := false
	defer func() { s.finishLeases(ctx, leases, dispatched) }()
	reservation, reserved := s.reserveToolCall(ctx, in)
	if reserved.failure != nil || reserved.repeat != nil {
		return reserved
	}
	credential, failure := s.loadCredential(ctx, in.authority, in.tool)
	if failure != nil {
		return execution{failure: s.refuseToolIdentity(ctx, in, reservation, failure)}
	}
	defer clear(credential)
	arguments, state, refusal := s.guardArguments(ctx, in, reservation, s.toolGuardrails(in))
	if refusal != nil {
		return execution{reservation: reservation, blocked: refusal}
	}
	dispatchedResult := s.dispatchToolCall(ctx, in, credential, reservation, arguments)
	dispatched = dispatchedResult.dispatched
	if dispatchedResult.failure != nil {
		return execution{failure: dispatchedResult.failure, certification: dispatchedResult.certification}
	}
	if dispatchedResult.callErr != nil {
		evidence := AttemptEvidence{
			ServerID: in.tool.ServerID, ServerRevisionID: in.tool.ServerRevisionID,
			Status: StatusOutcomeUnknown, IsError: false,
			ResultDigest: digestBytes([]byte(string(StatusOutcomeUnknown) + ":" + reservation.DispatchID)),
			StartedAt:    dispatchedResult.started, CompletedAt: dispatchedResult.completed,
			GuardrailDecisions: stateDecisions(state),
		}
		_ = s.Ledger.MarkUnknown(context.WithoutCancel(ctx), reservation, evidence)
		return execution{reservation: reservation, outcomeUnknown: true, unknownMessage: "The tool call may have run upstream, but its outcome could not be confirmed.", listChanged: dispatchedResult.listChanged, certification: dispatchedResult.certification}
	}
	return s.settleToolCall(ctx, in, reservation, dispatchedResult, state)
}
