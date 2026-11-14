package gateway

import (
	"context"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/release"
)

type delegationReauthenticator interface {
	ReauthenticateDelegation(context.Context, string, string) (*keyauthority.Authority, error)
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
	delegate, ok := s.deps.Runtime.(delegationReauthenticator)
	if !ok {
		return nil, release.ErrInvalidKey
	}
	return delegate.ReauthenticateDelegation(ctx, token, x.request.delegationAssertion)
}
