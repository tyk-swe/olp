package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
)

type executeInput struct {
	authority access.Authority
	resolved  resolvedToolset
	tool      routing.MCPToolsetTool
	arguments json.RawMessage
	releaseID string
}

type execution struct {
	reservation    *Reservation
	result         mcpservers.ToolResult
	failure        *apiError
	retryAfter     time.Duration
	outcomeUnknown bool
	unknownMessage string
}

func (s *Server) loadCredential(ctx context.Context, tool routing.MCPToolsetTool) ([]byte, *apiError) {
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
	reservation, err := s.Ledger.Reserve(ctx, ReserveInput{
		Authority: in.authority, ToolsetID: in.resolved.id, ToolsetRevisionID: in.resolved.revisionID,
		ReleaseID: in.releaseID, ToolAlias: in.tool.Alias, InputDigest: s.toolInputDigest(in.arguments),
	})
	if err != nil {
		var exceeded *limits.ExceededError
		if errors.As(err, &exceeded) {
			return nil, execution{failure: fail(http.StatusTooManyRequests, "budget_exceeded", "rate_limit_error", "The call exceeds an enforced budget, so it was not dispatched."), retryAfter: exceeded.RetryAfter}
		}
		return nil, execution{failure: errLedger}
	}
	if err = s.Runtime.AuthorizeAgentResource(in.authority, "tools", in.resolved.id, in.resolved.revisionID, *in.authority.ProjectID); err != nil {
		s.release(reservation)
		if errors.Is(err, runtime.ErrStaleAuthority) {
			return nil, execution{failure: fail(http.StatusServiceUnavailable, "authority_unavailable", "api_error", "Key authority is unavailable; retry shortly.")}
		}
		return nil, execution{failure: fail(http.StatusForbidden, "permission_denied", "permission_error", "This API key may no longer use the toolset.")}
	}
	return reservation, execution{}
}

func (s *Server) dispatchToolCall(ctx context.Context, tool routing.MCPToolsetTool, credential []byte, reservation *Reservation, arguments json.RawMessage) (mcpservers.ToolResult, time.Time, time.Time, error, *apiError) {
	transport, err := mcpservers.DialTransport(ctx, s.Egress, tool.Endpoint, credential, mcpservers.TransportOptions{ClientName: "openllmproxy-tools", Timeout: defaultCallTimeout})
	if err != nil {
		s.release(reservation)
		return mcpservers.ToolResult{}, time.Time{}, time.Time{}, nil, fail(http.StatusBadGateway, "upstream_unavailable", "api_error", "The upstream MCP server could not be reached, so the tool was not called.")
	}
	defer transport.Close(context.WithoutCancel(ctx))
	if err = s.Ledger.MarkDispatched(ctx, reservation); err != nil {
		s.release(reservation)
		return mcpservers.ToolResult{}, time.Time{}, time.Time{}, nil, errLedger
	}
	started := time.Now()
	result, callErr := transport.CallTool(ctx, tool.Name, arguments)
	completed := time.Now()
	return result, started, completed, callErr, nil
}

func (s *Server) settleToolCall(ctx context.Context, tool routing.MCPToolsetTool, reservation *Reservation, result mcpservers.ToolResult, started, completed time.Time) execution {
	encoded, _ := json.Marshal(result)
	status := StatusSucceeded
	if result.IsError {
		status = StatusUpstreamError
	}
	evidence := AttemptEvidence{
		ServerID: tool.ServerID, ServerRevisionID: tool.ServerRevisionID,
		Status: status, IsError: result.IsError, ResultDigest: digestBytes(encoded),
		StartedAt: started, CompletedAt: completed,
	}
	if err := s.Ledger.Settle(context.WithoutCancel(ctx), reservation, evidence); err != nil {
		unknownEvidence := AttemptEvidence{
			ServerID: tool.ServerID, ServerRevisionID: tool.ServerRevisionID,
			Status: StatusOutcomeUnknown, IsError: result.IsError, ResultDigest: digestBytes(encoded),
			StartedAt: started, CompletedAt: completed,
		}
		_ = s.Ledger.MarkUnknown(context.WithoutCancel(ctx), reservation, unknownEvidence)
		return execution{reservation: reservation, outcomeUnknown: true, unknownMessage: "The tool call ran upstream, but its accounting could not be settled."}
	}
	return execution{reservation: reservation, result: result}
}

func (s *Server) execute(parent context.Context, in executeInput) execution {
	credential, failure := s.loadCredential(parent, in.tool)
	if failure != nil {
		return execution{failure: failure}
	}
	if credential != nil {
		defer clear(credential)
	}
	ctx, cancel := context.WithTimeout(parent, defaultCallTimeout)
	defer cancel()
	reservation, reserved := s.reserveToolCall(ctx, in)
	if reserved.failure != nil {
		return reserved
	}
	result, started, completed, callErr, failure := s.dispatchToolCall(ctx, in.tool, credential, reservation, in.arguments)
	if failure != nil {
		return execution{failure: failure}
	}
	if callErr != nil {
		evidence := AttemptEvidence{
			ServerID: in.tool.ServerID, ServerRevisionID: in.tool.ServerRevisionID,
			Status: StatusOutcomeUnknown, IsError: false,
			ResultDigest: digestBytes([]byte(string(StatusOutcomeUnknown) + ":" + reservation.DispatchID)),
			StartedAt:    started, CompletedAt: completed,
		}
		_ = s.Ledger.MarkUnknown(context.WithoutCancel(ctx), reservation, evidence)
		return execution{reservation: reservation, outcomeUnknown: true, unknownMessage: "The tool call may have run upstream, but its outcome could not be confirmed."}
	}
	return s.settleToolCall(ctx, in.tool, reservation, result, started, completed)
}
