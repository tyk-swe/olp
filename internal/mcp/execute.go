package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents/runs"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/mcp/sessions"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

const toolRateLeaseTTL = 150 * time.Second

func toolPerKeyLookup(authority access.Authority, serverID string) string {
	sum := sha256.Sum256([]byte(authority.LimitsLookup() + "\x00" + serverID))
	return "tk_" + hex.EncodeToString(sum[:16])
}

func toolPerServerLookup(serverID string) string {
	return "ts_" + strings.ReplaceAll(serverID, "-", "")
}

func toolRateRequests(in CallRequest) []limits.Request {
	effective := routing.EffectiveToolLimits(in.Tool.CallsPerMinute, in.Tool.MaxConcurrency, in.Tool.PerKeyCallsPerMinute, in.Tool.PerKeyMaxConcurrency)
	requests := []limits.Request{
		{
			CostOwnerID: in.Authority.ID, LookupID: toolPerKeyLookup(in.Authority, in.Tool.ServerID),
			RequestsPerMinute: &effective.PerKeyCallsPerMinute,
			MaxConcurrency:    &effective.PerKeyMaxConcurrency,
			LeaseTTL:          toolRateLeaseTTL,
		},
		{
			CostOwnerID:       in.Tool.ServerID,
			LookupID:          toolPerServerLookup(in.Tool.ServerID),
			RequestsPerMinute: &effective.CallsPerMinute,
			MaxConcurrency:    &effective.MaxConcurrency,
			LeaseTTL:          toolRateLeaseTTL,
		},
	}
	if in.RunID != "" {
		return requests
	}
	return append(requests, toolCallerRate(in.Authority), toolGroupRate(in.Authority), toolAgentRate(in.Authority))
}

func toolCallerRate(authority access.Authority) limits.Request {
	if authority.Policy.RequestsPerMinute == nil && authority.Policy.MaxConcurrency == nil {
		return limits.Request{}
	}
	return limits.Request{CostOwnerID: authority.ID, LookupID: authority.LimitsLookup(), RequestsPerMinute: authority.Policy.RequestsPerMinute, MaxConcurrency: authority.Policy.MaxConcurrency, LeaseTTL: toolRateLeaseTTL}
}

func toolGroupRate(authority access.Authority) limits.Request {
	if authority.BudgetGroupID == nil || (authority.BudgetGroupRPM == nil && authority.BudgetGroupConcurrency == nil) {
		return limits.Request{}
	}
	return limits.Request{CostOwnerID: *authority.BudgetGroupID, LookupID: limits.BudgetGroupLookup(*authority.BudgetGroupID), RequestsPerMinute: authority.BudgetGroupRPM, MaxConcurrency: authority.BudgetGroupConcurrency, LeaseTTL: toolRateLeaseTTL}
}

func toolAgentRate(authority access.Authority) limits.Request {
	if authority.DelegationAgentLimits == nil || (authority.DelegationAgentLimits.Limits.RequestsPerMinute == nil && authority.DelegationAgentLimits.Limits.MaxConcurrency == nil) {
		return limits.Request{}
	}
	agent := authority.DelegationAgentLimits
	return limits.Request{CostOwnerID: agent.ID, LookupID: agent.LookupID, RequestsPerMinute: agent.Limits.RequestsPerMinute, MaxConcurrency: agent.Limits.MaxConcurrency, LeaseTTL: toolRateLeaseTTL}
}

func (s *Executor) reserveToolRate(ctx context.Context, in CallRequest) ([]*limits.Lease, *Failure, time.Duration) {
	limiter := s.limiter
	if limiter == nil {
		return nil, refusal("limits_unavailable", "Tool call limits are unavailable, so the call was not dispatched."), 0
	}
	requests := toolRateRequests(in)
	leases := make([]*limits.Lease, 0, len(requests))
	for _, request := range requests {
		if !request.HasRateLimits() {
			continue
		}
		lease, err := limiter.Reserve(ctx, request)
		if err != nil {
			s.finishLeases(ctx, leases, false)
			var exceeded *limits.ExceededError
			if errors.As(err, &exceeded) {
				return nil, refusal("tool_rate_limited", "The call exceeds a tool rate or concurrency limit, so it was not dispatched."), exceeded.RetryAfter
			}
			return nil, refusal("limits_unavailable", "Tool call limits are unavailable, so the call was not dispatched."), 0
		}
		if lease != nil {
			leases = append(leases, lease)
		}
	}
	return leases, nil, 0
}

type CallRequest struct {
	Authority         access.Authority
	Approval          *runs.WorkflowApproval
	RunID             string
	Ordinal           int
	ToolsetID         string
	ToolsetRevisionID string
	Tool              routing.MCPToolsetTool
	Arguments         json.RawMessage
	ReleaseID         string
	CallID            string
	Release           *release.Release
	Policies          ToolPolicies
}

type dispatchResult struct {
	result        upstream.ToolResult
	started       time.Time
	completed     time.Time
	listChanged   bool
	callErr       error
	certification sessions.Certification
}

func (s *Executor) loadOriginalCredential(ctx context.Context, authority access.Authority, tool routing.MCPToolsetTool) ([]byte, *Failure) {
	if tool.OAuth != nil {
		if s.grants == nil {
			return nil, refusal("user_grant_required", "Enroll a usable personal grant before calling this tool.")
		}
		credential, err := s.grants.Resolve(ctx, authority, tool)
		if err != nil {
			return nil, refusal("user_grant_required", "Enroll a usable personal grant before calling this tool.")
		}
		return credential, nil
	}
	if tool.CredentialID == nil {
		return nil, nil
	}
	if s.credentials == nil {
		return nil, refusal("credential_unavailable", "The registered credential is unavailable, so the call was not dispatched.")
	}
	credential, err := s.credentials(ctx, *tool.CredentialID)
	if err != nil || len(credential) == 0 {
		return nil, refusal("credential_unavailable", "The registered credential is unavailable, so the call was not dispatched.")
	}
	return credential, nil
}

func (s *Executor) reserveToolCall(ctx context.Context, in CallRequest) (*accounting.Reservation, Execution) {
	reservedResult, err := s.ledger.Reserve(ctx, accounting.ReserveInput{
		Authority: in.Authority, Approval: in.Approval, RunID: in.RunID, Ordinal: in.Ordinal, ToolsetID: in.ToolsetID, ToolsetRevisionID: in.ToolsetRevisionID,
		ReleaseID: in.ReleaseID, ToolAlias: in.Tool.Alias, InputDigest: s.toolInputDigest(in.Arguments),
		CallDigest: s.callDigest(in.Authority, in.CallID), UnitPrice: cloneUnitPrice(in.Tool.UnitPrice),
		ServerID: in.Tool.ServerID, ServerRevisionID: in.Tool.ServerRevisionID,
	})
	if err == nil && reservedResult.Repeat != nil {
		prior := *reservedResult.Repeat
		if !prior.Conflict && (prior.Status == accounting.StatusSucceeded || prior.Status == accounting.StatusUpstreamError) {
			if result, found := s.results.find(prior.AttemptID, s.now()); found {
				return nil, Execution{Outcome: Replayed{Receipt: Receipt{AttemptID: prior.AttemptID, DispatchID: prior.DispatchID}, Result: result}}
			}
		}
		return nil, Execution{Outcome: ReplayUnavailable{Call: prior, Reason: repeatOutcome(&prior, false)}}
	}
	if err != nil {
		if errors.Is(err, accounting.ErrUnpricedToolCall) {
			return nil, Execution{Outcome: Refused{Failure: refusal("tool_unpriced", "The tool has no price and an enforced budget needs a finite bound, so the call was not dispatched."), RetryAfter: 0}}
		}
		var exceeded *limits.ExceededError
		if errors.As(err, &exceeded) {
			return nil, Execution{Outcome: Refused{Failure: refusal("budget_exceeded", "The call exceeds an enforced budget, so it was not dispatched."), RetryAfter: exceeded.RetryAfter}}
		}
		return nil, Execution{Outcome: Refused{Failure: failureLedger, RetryAfter: 0}}
	}
	reservation := reservedResult.Reservation
	if reservation == nil {
		return nil, Execution{Outcome: Refused{Failure: failureLedger, RetryAfter: 0}}
	}
	if failure := s.currentToolAuthority(in); failure != nil {
		s.release(reservation)
		return nil, Execution{Outcome: Refused{Failure: failure, RetryAfter: 0}}
	}
	return reservation, Execution{}
}

func (s *Executor) dispatchToolCall(ctx context.Context, in CallRequest, credential []byte, reservation *accounting.Reservation, arguments json.RawMessage) (dispatchResult, *Failure) {
	tool := in.Tool
	transport, err := s.connect(ctx, tool.Endpoint, credential, upstream.TransportOptions{ClientName: "openllmproxy-tools", Timeout: defaultCallTimeout, Transport: tool.Transport, Stdio: tool.Stdio})
	if err != nil {
		s.release(reservation)
		return dispatchResult{certification: sessions.Invalid}, refusal("upstream_unavailable", "The upstream MCP server could not be reached, so the tool was not called.")
	}
	defer transport.Close(context.WithoutCancel(ctx))
	switch state := certifyTransport(ctx, transport, credential, tool.CatalogDigest); state {
	case sessions.Invalid:
		s.release(reservation)
		return dispatchResult{certification: state}, refusal("upstream_unavailable", "The upstream MCP server catalog could not be verified, so the tool was not called.")
	case sessions.Drifted:
		s.release(reservation)
		return dispatchResult{certification: state}, failureDefinitionChanged
	}
	if failure := s.currentToolAuthority(in); failure != nil {
		return dispatchResult{}, s.refuseToolIdentity(ctx, in, reservation, failure)
	}
	if tool.OAuth != nil {
		if s.grants == nil {
			return dispatchResult{}, s.refuseToolIdentity(ctx, in, reservation, refusal("user_grant_required", "Enroll a usable personal grant before calling this tool."))
		}
		if err := s.grants.CheckGrant(ctx, in.Authority, tool); err != nil {
			return dispatchResult{}, s.refuseToolIdentity(ctx, in, reservation, refusal("user_grant_required", "Enroll a usable personal grant before calling this tool."))
		}
	}
	if in.Authority.TaskCapability != nil {
		if err := s.authority.AdmitTaskCapabilityAttempt(ctx, in.Authority, "tools", in.ToolsetID, in.ToolsetRevisionID, in.Authority.ProjectID, reservation.DispatchID); err != nil {
			if errors.Is(err, release.ErrTaskAttemptLimit) {
				return dispatchResult{}, s.refuseToolIdentity(ctx, in, reservation, refusal("task_limit_exceeded", "The task attempt limit was exceeded."))
			}
			return dispatchResult{}, s.refuseToolIdentity(ctx, in, reservation, failurePermissionDenied)
		}
	}
	started := s.now()
	dispatchedEvidence := accounting.AttemptEvidence{
		ServerID: tool.ServerID, ServerRevisionID: tool.ServerRevisionID,
		Status: accounting.StatusDispatched, StartedAt: started, IdentityDecisions: toolIdentityDecisions(tool, nil),
	}
	if err = s.ledger.MarkDispatched(ctx, reservation, dispatchedEvidence); err != nil {
		s.release(reservation)
		return dispatchResult{certification: sessions.Certified}, failureLedger
	}
	result, callErr := transport.CallTool(ctx, tool.Name, arguments)
	return dispatchResult{result: result, started: started, completed: s.now(), listChanged: transport.ListChanged(), callErr: callErr, certification: sessions.Certified}, nil
}

func (s *Executor) settleToolCall(ctx context.Context, in CallRequest, reservation *accounting.Reservation, dispatched dispatchResult, state *toolGuardrailState) Execution {
	guarded, decisions, refusal := s.guardResult(ctx, in, reservation, state, dispatched.result)
	result, status := func() (upstream.ToolResult, accounting.Status) {
		if refusal != nil {
			return dispatched.result, accounting.StatusGuardrailBlocked
		}
		return guarded, completedToolStatus(guarded)
	}()
	encoded, _ := json.Marshal(result)
	resultDigest := digestBytes(encoded)
	evidenceFor := func(status accounting.Status) accounting.AttemptEvidence {
		return accounting.AttemptEvidence{
			ServerID: in.Tool.ServerID, ServerRevisionID: in.Tool.ServerRevisionID,
			Status: status, IsError: result.IsError, ResultDigest: resultDigest,
			StartedAt: dispatched.started, CompletedAt: dispatched.completed,
			GuardrailDecisions: decisions, IdentityDecisions: toolIdentityDecisions(in.Tool, nil),
		}
	}
	if err := s.ledger.Settle(context.WithoutCancel(ctx), reservation, evidenceFor(status)); err != nil {
		_ = s.ledger.MarkUnknown(context.WithoutCancel(ctx), reservation, evidenceFor(accounting.StatusOutcomeUnknown))
		return Execution{Outcome: Uncertain{Receipt: receipt(reservation), Message: "The tool call ran upstream, but its accounting could not be settled."}, ListChanged: dispatched.listChanged, Certification: dispatched.certification}
	}
	if refusal != nil {
		return Execution{Outcome: Blocked{Receipt: receipt(reservation), Failure: guardrailRefusal(refusal)}, ListChanged: dispatched.listChanged, Certification: dispatched.certification}
	}
	s.results.retain(reservation.AttemptID, guarded, s.now())
	return Execution{Outcome: Completed{Receipt: receipt(reservation), Result: guarded}, ListChanged: dispatched.listChanged, Certification: dispatched.certification}
}

func completedToolStatus(result upstream.ToolResult) accounting.Status {
	if result.IsError {
		return accounting.StatusUpstreamError
	}
	return accounting.StatusSucceeded
}

func (s *Executor) Execute(parent context.Context, in CallRequest) Execution {
	if failure := executeCallIDFailure(in.CallID); failure != nil {
		return Execution{Outcome: Refused{Failure: failure}}
	}
	if !s.available() || in.Authority.ProjectID == nil {
		return Execution{Outcome: Refused{Failure: failureLedger}}
	}
	ctx, cancel := context.WithTimeout(parent, defaultCallTimeout)
	defer cancel()
	leases, failure, retryAfter := s.reserveToolRate(ctx, in)
	if failure != nil {
		return Execution{Outcome: Refused{Failure: failure, RetryAfter: retryAfter}}
	}
	result, dispatched := s.executeReserved(ctx, in)
	s.finishLeases(ctx, leases, dispatched)
	return result
}

func (s *Executor) executeReserved(ctx context.Context, in CallRequest) (Execution, bool) {
	reservation, reserved := s.reserveToolCall(ctx, in)
	if reserved.Outcome != nil {
		return reserved, false
	}
	credential, failure := s.loadCredential(ctx, in.Authority, in.Tool)
	if failure != nil {
		return Execution{Outcome: Refused{Failure: s.refuseToolIdentity(ctx, in, reservation, failure)}}, false
	}
	defer clear(credential)
	arguments, state, refusal := s.guardArguments(ctx, in, reservation, s.toolGuardrails(in))
	if refusal != nil {
		return Execution{Outcome: Blocked{Receipt: receipt(reservation), Failure: refusal}}, false
	}
	dispatched, dispatchFailure := s.dispatchToolCall(ctx, in, credential, reservation, arguments)
	if dispatchFailure != nil {
		return Execution{Outcome: Refused{Failure: dispatchFailure}, Certification: dispatched.certification}, false
	}
	if dispatched.callErr != nil {
		evidence := accounting.AttemptEvidence{
			ServerID: in.Tool.ServerID, ServerRevisionID: in.Tool.ServerRevisionID,
			Status: accounting.StatusOutcomeUnknown, ResultDigest: digestBytes([]byte(string(accounting.StatusOutcomeUnknown) + ":" + reservation.DispatchID)),
			StartedAt: dispatched.started, CompletedAt: dispatched.completed, GuardrailDecisions: stateDecisions(state),
		}
		if err := s.ledger.MarkUnknown(context.WithoutCancel(ctx), reservation, evidence); err != nil {
			s.log.Error("record uncertain tool outcome", "attempt_id", reservation.AttemptID, "error", err)
		}
		return Execution{Outcome: Uncertain{Receipt: receipt(reservation), Message: "The tool call may have run upstream, but its outcome could not be confirmed."}, ListChanged: dispatched.listChanged, Certification: dispatched.certification}, true
	}
	return s.settleToolCall(ctx, in, reservation, dispatched, state), true
}
