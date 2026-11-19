package kernel

import (
	"context"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/release"
)

type DelegationReauthenticator interface {
	ReauthenticateDelegation(context.Context, string, string) (*keyauthority.Authority, error)
}

func (s *Server) reauthenticateSession(ctx context.Context, x *Call, token string) (*keyauthority.Authority, error) {
	if x.authority.TaskCapability != nil {
		auth := s.Deps.TaskCapabilityAuthenticator
		if auth == nil {
			return nil, release.ErrInvalidKey
		}
		return auth.AuthenticateTaskCapability(ctx, token)
	}
	if x.authority.DelegationAgentID == "" {
		return s.Deps.Runtime.Authenticate(token)
	}
	delegate := s.Deps.DelegationReauthenticator
	if delegate == nil {
		return nil, release.ErrInvalidKey
	}
	return delegate.ReauthenticateDelegation(ctx, token, x.request.delegationAssertion)
}
