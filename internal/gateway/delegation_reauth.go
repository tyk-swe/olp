package gateway

import (
	"context"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/release"
)

type DelegationReauthenticator interface {
	ReauthenticateDelegation(context.Context, string, string) (*keyauthority.Authority, error)
}

func (s *Server) reauthenticateSession(ctx context.Context, x *call, token string) (*keyauthority.Authority, error) {
	if x.authority.TaskCapability != nil {
		auth := s.deps.TaskCapabilityAuthenticator
		if auth == nil {
			return nil, release.ErrInvalidKey
		}
		return auth.AuthenticateTaskCapability(ctx, token)
	}
	if x.authority.DelegationAgentID == "" {
		return s.deps.Runtime.Authenticate(token)
	}
	delegate := s.deps.DelegationReauthenticator
	if delegate == nil {
		return nil, release.ErrInvalidKey
	}
	return delegate.ReauthenticateDelegation(ctx, token, x.request.delegationAssertion)
}
