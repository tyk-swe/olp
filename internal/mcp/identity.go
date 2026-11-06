package mcp

import (
	"context"
	"errors"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/workload"
)

func hookOutcome(configured, denied bool) accounting.IdentityOutcome {
	if !configured {
		return accounting.IdentityNotConfigured
	}
	if denied {
		return accounting.IdentityDenied
	}
	return accounting.IdentityPassed
}

func toolIdentityDecisions(tool routing.MCPToolsetTool, failure *Failure) []accounting.IdentityDecision {
	decisions := []accounting.IdentityDecision{{Hook: accounting.RegisteredServerHook, Outcome: hookOutcome(true, failure != nil && (failure.Code == "permission_denied" || failure.Code == "authority_unavailable"))}, {Hook: accounting.UserPermissionHook, Outcome: hookOutcome(tool.OAuth != nil, failure != nil && failure.Code == "user_grant_required")}, {Hook: accounting.DelegatedSigningHook, Outcome: hookOutcome(tool.Delegation != nil, failure != nil)}}
	if tool.OAuth != nil && tool.OAuth.EntraOBO != nil {
		return append(decisions, accounting.IdentityDecision{Hook: accounting.EntraOBOHook, Outcome: hookOutcome(true, failure != nil)})
	}
	return decisions
}

func (s *Executor) loadCredential(ctx context.Context, authority keyauthority.Authority, tool routing.MCPToolsetTool) ([]byte, *Failure) {
	if tool.OAuth != nil && tool.OAuth.EntraOBO != nil {
		assertion, err := s.authority.VerifyEntraUser(ctx, workload.RequestAssertion(ctx), authority, tool.OAuth.EntraOBO.Issuer, tool.OAuth.ClientID)
		if err != nil || s.grants == nil {
			return nil, refusal("user_grant_required", "A verified delegated user assertion is required.")
		}
		return s.loadEntraCredential(ctx, authority, tool, assertion)
	}
	credential, failure := s.loadOriginalCredential(ctx, authority, tool)
	if failure != nil {
		return nil, failure
	}
	if tool.Delegation == nil {
		return credential, nil
	}
	defer clear(credential)
	if s.grants == nil {
		return nil, refusal("delegation_unavailable", "Delegated signing is unavailable for this tool.")
	}
	delegated, err := s.grants.Delegate(ctx, authority, tool)
	if err != nil {
		return nil, refusal("delegation_unavailable", "Delegated signing is unavailable for this tool.")
	}
	return delegated, nil
}

func (s *Executor) currentToolAuthority(in CallRequest) *Failure {
	err := s.authority.AuthorizeMCPTool(in.Authority, in.ToolsetID, in.ToolsetRevisionID, *in.Authority.ProjectID, in.Tool.ServerID, in.Tool.ServerRevisionID)
	if err == nil {
		return nil
	}
	if errors.Is(err, release.ErrStaleAuthority) {
		return refusal("authority_unavailable", "Key authority is unavailable; retry shortly.")
	}
	return failurePermissionDenied
}

func (s *Executor) refuseToolIdentity(ctx context.Context, in CallRequest, reservation *accounting.Reservation, failure *Failure) *Failure {
	now := s.now()
	evidence := accounting.AttemptEvidence{ServerID: in.Tool.ServerID, ServerRevisionID: in.Tool.ServerRevisionID, Status: accounting.StatusGuardrailBlocked, StartedAt: now, CompletedAt: now, IdentityDecisions: toolIdentityDecisions(in.Tool, failure)}
	if err := s.ledger.RefuseUndispatched(context.WithoutCancel(ctx), reservation, evidence); err != nil {
		s.release(reservation)
		return failureLedger
	}
	return failure
}

func (s *Executor) loadEntraCredential(ctx context.Context, authority keyauthority.Authority, tool routing.MCPToolsetTool, assertion workload.UserAssertion) ([]byte, *Failure) {
	credential, err := s.grants.ResolveEntra(ctx, authority, tool, assertion)
	if err != nil {
		return nil, refusal("user_grant_required", "A verified delegated user grant is required.")
	}
	return credential, nil
}
