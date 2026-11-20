package kernel

import (
	"context"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

type DelegationReauthenticator interface {
	ReauthenticateDelegation(context.Context, string, string) (*keyauthority.Authority, error)
}

func (s *Server) sessionAuthorityValue(ctx context.Context, x *Call, token, keyID string) (*keyauthority.Authority, bool) {
	authority, err := s.ReauthenticateSession(ctx, x, token)
	if err != nil || authority == nil || authority.ID != keyID || !authority.Allows("inference", x.Route.Slug, x.Route.ProjectID, s.Now()) || !authority.AllowsEndUser(x.endUserDigest) || (authority.WorkloadIssuerID != nil && authority.EndUserDigest != x.endUserDigest) || !authority.AllowsAttribution(x.attribution) || !authority.AllowsClientIP(x.request.clientIP) {
		return nil, false
	}
	return authority, true
}

func (s *Server) SessionStillAuthorized(ctx context.Context, x *Call, token, keyID string) bool {
	_, ok := s.sessionAuthorityValue(ctx, x, token, keyID)
	return ok
}

func (s *Server) SessionAuthority(ctx context.Context, x *Call, token, keyID string, p *Pin) *fault.Error {
	authority, ok := s.sessionAuthorityValue(ctx, x, token, keyID)
	if !ok {
		return fault.PermissionError("key_revoked", "The realtime session no longer has route authority.")
	}
	if eligibility := s.PinEligibility(p); eligibility != routing.Eligible {
		return fault.PermissionError("provider_credential_"+string(eligibility), "The realtime provider credential is no longer eligible.")
	}
	current := *authority
	current.EndUserDigest = x.endUserDigest
	if e := s.UnboundedPinLimits(x.AdmissionAuthority(current), p, x.Route); e != nil {
		return e
	}
	return nil
}

func (s *Server) ReauthenticateSession(ctx context.Context, x *Call, token string) (*keyauthority.Authority, error) {
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
