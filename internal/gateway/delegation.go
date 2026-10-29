package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/runtime"
)

type delegationRuntime interface {
	CreateDelegation(context.Context, access.Authority, access.Authority, runtime.DelegationBounds) (runtime.DelegationGrant, error)
	AuthenticateDelegation(context.Context, string, string) (*access.Authority, error)
	ReauthenticateDelegation(context.Context, string, string) (*access.Authority, error)
}

type memberConsentRuntime interface {
	ExchangeMemberConsent(context.Context, access.Authority, string, runtime.DelegationBounds) (runtime.DelegationGrant, error)
}

func (s *Server) reauthenticateSession(ctx context.Context, x *call, token string) (*access.Authority, error) {
	if x.authority.DelegationAgentID == "" {
		return s.Runtime.Authenticate(token)
	}
	delegate, ok := s.Runtime.(delegationRuntime)
	if !ok {
		return nil, runtime.ErrInvalidKey
	}
	return delegate.ReauthenticateDelegation(ctx, token, x.request.delegationAssertion)
}

func (s *Server) createDelegation(w http.ResponseWriter, r *http.Request) {
	s.begin(w, r)
	if !s.admit(r.Context()) {
		writeError(w, overloaded)
		return
	}
	defer s.release(r.Context())
	delegate, ok := s.Runtime.(delegationRuntime)
	if !ok {
		writeError(w, serverError(http.StatusServiceUnavailable, "authority_unavailable", "Delegation is unavailable."))
		return
	}
	assertion, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, authenticationError("invalid_api_key", "Provide a workload identity."))
		return
	}
	var bounds runtime.DelegationBounds
	if err := access.DecodeUnique(r, &bounds, 16<<10); err != nil {
		writeError(w, invalidRequest("invalid_request", "Provide bounded delegation scopes and resources.", nil))
		return
	}
	agent, failure := consumerauth.Authenticate(s.Runtime, r, assertion, delegationScope(bounds), s.cfg.TrustedProxies, s.now())
	if failure != nil {
		writeError(w, consumerFailure(failure))
		return
	}
	if consent := strings.TrimSpace(r.Header.Get("X-OLP-Delegation-Consent")); consent != "" {
		if strings.TrimSpace(r.Header.Get("X-OLP-Delegating-Key")) != "" {
			writeError(w, invalidRequest("invalid_request", "Provide either a delegating key or member consent.", nil))
			return
		}
		member, ok := s.Runtime.(memberConsentRuntime)
		if !ok {
			writeError(w, serverError(http.StatusServiceUnavailable, "authority_unavailable", "Delegation is unavailable."))
			return
		}
		grant, err := member.ExchangeMemberConsent(r.Context(), agent, consent, bounds)
		writeDelegationGrant(w, grant, err)
		return
	}
	key, failure := consumerauth.Authenticate(s.Runtime, r, strings.TrimSpace(r.Header.Get("X-OLP-Delegating-Key")), "delegate", s.cfg.TrustedProxies, s.now())
	if failure != nil {
		writeError(w, consumerFailure(failure))
		return
	}
	grant, err := delegate.CreateDelegation(r.Context(), agent, key, bounds)
	writeDelegationGrant(w, grant, err)
}

func delegationScope(bounds runtime.DelegationBounds) string {
	if slices.Contains(bounds.Scopes, "tools") {
		return "tools"
	}
	return "inference"
}

func writeDelegationGrant(w http.ResponseWriter, grant runtime.DelegationGrant, err error) {
	if errors.Is(err, runtime.ErrDelegationBounds) {
		writeError(w, invalidRequest("invalid_request", "Provide bounded delegation scopes and resources.", nil))
		return
	}
	if errors.Is(err, runtime.ErrInvalidKey) {
		writeError(w, authenticationError("invalid_api_key", "The member consent is invalid or expired."))
		return
	}
	var problem *access.Problem
	if errors.As(err, &problem) && problem.Status == http.StatusForbidden {
		writeError(w, permissionError("permission_denied", "The principals cannot delegate the requested rights."))
		return
	}
	if err != nil {
		writeError(w, serverError(http.StatusServiceUnavailable, "authority_unavailable", "Delegation is unavailable."))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(grant)
}
