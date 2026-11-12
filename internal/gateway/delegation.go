package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/release"
)

type delegationRuntime interface {
	CreateDelegation(context.Context, keyauthority.Authority, keyauthority.Authority, release.DelegationBounds) (release.DelegationGrant, error)
	AuthenticateDelegation(context.Context, string, string) (*keyauthority.Authority, error)
	ReauthenticateDelegation(context.Context, string, string) (*keyauthority.Authority, error)
}

type memberConsentRuntime interface {
	ExchangeMemberConsent(context.Context, keyauthority.Authority, string, release.DelegationBounds) (release.DelegationGrant, error)
}

func (s *Server) reauthenticateSession(ctx context.Context, x *call, token string) (*keyauthority.Authority, error) {
	if x.authority.TaskCapability != nil {
		auth, ok := s.deps.Runtime.(consumerauth.TaskCapabilityAuthenticator)
		if !ok {
			return nil, release.ErrInvalidKey
		}
		return auth.AuthenticateTaskCapability(ctx, token)
	}
	if x.authority.DelegationAgentID == "" {
		return s.deps.Runtime.Authenticate(token)
	}
	delegate, ok := s.deps.Runtime.(delegationRuntime)
	if !ok {
		return nil, release.ErrInvalidKey
	}
	return delegate.ReauthenticateDelegation(ctx, token, x.request.delegationAssertion)
}

func (s *Server) createDelegation(w http.ResponseWriter, r *http.Request) {
	s.begin(w, r)
	if !s.gate.Admit(r.Context()) {
		fault.WriteError(w, fault.Overloaded)
		return
	}
	defer s.gate.Release(r.Context())
	delegate, ok := s.deps.Runtime.(delegationRuntime)
	if !ok {
		fault.WriteError(w, fault.ServerError(http.StatusServiceUnavailable, "authority_unavailable", "Delegation is unavailable."))
		return
	}
	assertion, ok := consumerauth.Bearer(r.Header.Get("Authorization"))
	if !ok {
		fault.WriteError(w, fault.AuthenticationError("invalid_api_key", "Provide a workload identity."))
		return
	}
	var bounds release.DelegationBounds
	if err := access.DecodeUnique(r, &bounds, 16<<10); err != nil {
		fault.WriteError(w, fault.InvalidRequest("invalid_request", "Provide bounded delegation scopes and resources.", nil))
		return
	}
	agent, failure := consumerauth.Authenticate(s.deps.Runtime, r, assertion, delegationScope(bounds), s.cfg.TrustedProxies, s.now())
	if failure != nil {
		fault.WriteError(w, ingress.ConsumerFailure(failure))
		return
	}
	if consent := strings.TrimSpace(r.Header.Get("X-OLP-Delegation-Consent")); consent != "" {
		if strings.TrimSpace(r.Header.Get("X-OLP-Delegating-Key")) != "" {
			fault.WriteError(w, fault.InvalidRequest("invalid_request", "Provide either a delegating key or member consent.", nil))
			return
		}
		member, ok := s.deps.Runtime.(memberConsentRuntime)
		if !ok {
			fault.WriteError(w, fault.ServerError(http.StatusServiceUnavailable, "authority_unavailable", "Delegation is unavailable."))
			return
		}
		grant, err := member.ExchangeMemberConsent(r.Context(), agent, consent, bounds)
		writeDelegationGrant(w, grant, err)
		return
	}
	key, failure := consumerauth.Authenticate(s.deps.Runtime, r, strings.TrimSpace(r.Header.Get("X-OLP-Delegating-Key")), "delegate", s.cfg.TrustedProxies, s.now())
	if failure != nil {
		fault.WriteError(w, ingress.ConsumerFailure(failure))
		return
	}
	grant, err := delegate.CreateDelegation(r.Context(), agent, key, bounds)
	writeDelegationGrant(w, grant, err)
}

func delegationScope(bounds release.DelegationBounds) string {
	if slices.Contains(bounds.Scopes, "tools") {
		return "tools"
	}
	if slices.Contains(bounds.Scopes, "agents") {
		return "agents"
	}
	return "inference"
}

func writeDelegationGrant(w http.ResponseWriter, grant release.DelegationGrant, err error) {
	if errors.Is(err, release.ErrDelegationBounds) {
		fault.WriteError(w, fault.InvalidRequest("invalid_request", "Provide bounded delegation scopes and resources.", nil))
		return
	}
	if errors.Is(err, release.ErrInvalidKey) {
		fault.WriteError(w, fault.AuthenticationError("invalid_api_key", "The member consent is invalid or expired."))
		return
	}
	var problem *access.Problem
	if errors.As(err, &problem) && problem.Status == http.StatusForbidden {
		fault.WriteError(w, fault.PermissionError("permission_denied", "The principals cannot delegate the requested rights."))
		return
	}
	if err != nil {
		fault.WriteError(w, fault.ServerError(http.StatusServiceUnavailable, "authority_unavailable", "Delegation is unavailable."))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(grant)
}
