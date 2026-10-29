package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/runtime"
)

type delegationRuntime interface {
	CreateDelegation(context.Context, access.Authority, access.Authority, runtime.DelegationBounds) (runtime.DelegationGrant, error)
	AuthenticateDelegation(context.Context, string, string) (*access.Authority, error)
}

type delegationAuthenticator struct {
	runtime   delegationRuntime
	context   context.Context
	assertion string
}

func (a delegationAuthenticator) Authenticate(token string) (*access.Authority, error) {
	return a.runtime.AuthenticateDelegation(a.context, token, a.assertion)
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
	agent, failure := consumerauth.Authenticate(s.Runtime, r, assertion, "inference", s.cfg.TrustedProxies, s.now())
	if failure != nil {
		writeError(w, consumerFailure(failure))
		return
	}
	key, failure := consumerauth.Authenticate(s.Runtime, r, strings.TrimSpace(r.Header.Get("X-OLP-Delegating-Key")), "delegate", s.cfg.TrustedProxies, s.now())
	if failure != nil {
		writeError(w, consumerFailure(failure))
		return
	}
	var bounds runtime.DelegationBounds
	if err := access.DecodeUnique(r, &bounds, 16<<10); err != nil {
		writeError(w, invalidRequest("invalid_request", "Provide bounded delegation scopes and routes.", nil))
		return
	}
	grant, err := delegate.CreateDelegation(r.Context(), agent, key, bounds)
	if errors.Is(err, runtime.ErrDelegationBounds) {
		writeError(w, invalidRequest("invalid_request", "Provide bounded delegation scopes and routes.", nil))
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
