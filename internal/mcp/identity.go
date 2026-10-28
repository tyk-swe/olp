package mcp

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/workload"
)

type IdentityHook string
type IdentityOutcome string

const (
	RegisteredServerHook  IdentityHook    = "mcp_security"
	UserPermissionHook    IdentityHook    = "mcp_end_user_permission"
	DelegatedSigningHook  IdentityHook    = "mcp_jwt_signer"
	EntraOBOHook          IdentityHook    = "agent_365"
	IdentityPassed        IdentityOutcome = "passed"
	IdentityDenied        IdentityOutcome = "denied"
	IdentityNotConfigured IdentityOutcome = "not_configured"
)

type IdentityDecision struct {
	Hook    IdentityHook    `json:"hook"`
	Outcome IdentityOutcome `json:"outcome"`
}

func identityEvidence(decisions []IdentityDecision) []IdentityDecision {
	if decisions == nil {
		return []IdentityDecision{}
	}
	return decisions
}

func hookOutcome(configured, denied bool) IdentityOutcome {
	if !configured {
		return IdentityNotConfigured
	}
	if denied {
		return IdentityDenied
	}
	return IdentityPassed
}

func toolIdentityDecisions(tool routing.MCPToolsetTool, failure *apiError) []IdentityDecision {
	decisions := []IdentityDecision{{Hook: RegisteredServerHook, Outcome: hookOutcome(true, failure != nil && (failure.code == "permission_denied" || failure.code == "authority_unavailable"))}, {Hook: UserPermissionHook, Outcome: hookOutcome(tool.OAuth != nil, failure != nil && failure.code == "user_grant_required")}, {Hook: DelegatedSigningHook, Outcome: hookOutcome(tool.Delegation != nil, failure != nil)}}
	if tool.OAuth != nil && tool.OAuth.EntraOBO != nil {
		return append(decisions, IdentityDecision{Hook: EntraOBOHook, Outcome: hookOutcome(true, failure != nil)})
	}
	return decisions
}

func (s *Server) loadCredential(ctx context.Context, authority access.Authority, tool routing.MCPToolsetTool) ([]byte, *apiError) {
	if tool.OAuth != nil && tool.OAuth.EntraOBO != nil {
		verifier, ok := s.Runtime.(interface {
			VerifyEntraUser(context.Context, string, access.Authority, string, string) (workload.UserAssertion, error)
		})
		if !ok {
			return nil, fail(http.StatusForbidden, "user_grant_required", "permission_error", "A verified delegated user assertion is required.")
		}
		assertion, err := verifier.VerifyEntraUser(ctx, workload.RequestAssertion(ctx), authority, tool.OAuth.EntraOBO.Issuer, tool.OAuth.ClientID)
		if err != nil || s.Grants == nil {
			return nil, fail(http.StatusForbidden, "user_grant_required", "permission_error", "A verified delegated user assertion is required.")
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
	if s.Grants == nil {
		return nil, fail(http.StatusForbidden, "delegation_unavailable", "permission_error", "Delegated signing is unavailable for this tool.")
	}
	delegated, err := s.Grants.Delegate(ctx, authority, tool)
	if err != nil {
		return nil, fail(http.StatusForbidden, "delegation_unavailable", "permission_error", "Delegated signing is unavailable for this tool.")
	}
	return delegated, nil
}

func (s *Server) currentToolAuthority(in executeInput) *apiError {
	err := s.Runtime.AuthorizeAgentResource(in.authority, "tools", in.resolved.id, in.resolved.revisionID, *in.authority.ProjectID)
	if err == nil {
		return nil
	}
	if errors.Is(err, runtime.ErrStaleAuthority) {
		return fail(http.StatusServiceUnavailable, "authority_unavailable", "api_error", "Key authority is unavailable; retry shortly.")
	}
	if s.driftScoped(in.authority, in.resolved) {
		return nil
	}
	return errPermissionDenied
}

func (s *Server) refuseToolIdentity(ctx context.Context, in executeInput, reservation *Reservation, failure *apiError) *apiError {
	now := time.Now()
	evidence := AttemptEvidence{ServerID: in.tool.ServerID, ServerRevisionID: in.tool.ServerRevisionID, Status: StatusGuardrailBlocked, StartedAt: now, CompletedAt: now, IdentityDecisions: toolIdentityDecisions(in.tool, failure)}
	if err := s.Ledger.RefuseUndispatched(context.WithoutCancel(ctx), reservation, evidence); err != nil {
		s.release(reservation)
		return errLedger
	}
	return failure
}

func (s *Server) loadEntraCredential(ctx context.Context, authority access.Authority, tool routing.MCPToolsetTool, assertion workload.UserAssertion) ([]byte, *apiError) {
	credential, err := s.Grants.ResolveEntra(ctx, authority, tool, assertion)
	if err != nil {
		return nil, fail(http.StatusForbidden, "user_grant_required", "permission_error", "A verified delegated user grant is required.")
	}
	return credential, nil
}
