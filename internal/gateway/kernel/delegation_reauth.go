package kernel

import (
	"context"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

type DelegationReauthenticator interface {
	ReauthenticateDelegation(context.Context, string, string) (*keyauthority.Authority, error)
}

func (s *Server) sessionAuthorityValue(ctx context.Context, x *Call, token, keyID string) (*keyauthority.Authority, bool) {
	authority, err := s.reauthenticateSession(ctx, x, token)
	if err != nil || authority == nil || authority.ID != keyID || !authority.Allows("inference", x.Route.Slug, x.Route.ProjectID, s.Now()) || !authority.AllowsEndUser(x.endUserDigest) || (authority.WorkloadIssuerID != nil && authority.EndUserDigest != x.endUserDigest) || !authority.AllowsAttribution(x.attribution) || !authority.AllowsClientIP(x.request.clientIP) {
		return nil, false
	}
	return authority, true
}

func (s *Server) SessionStillAuthorized(ctx context.Context, x *Call, token, keyID string) bool {
	_, ok := s.sessionAuthorityValue(ctx, x, token, keyID)
	return ok
}

type SessionAuthStatus int

const (
	SessionAuthorized SessionAuthStatus = iota
	SessionAuthorityRevoked
	SessionCredentialIneligible
	SessionBudgetChanged
)

type SessionAuthCheck struct {
	Status      SessionAuthStatus
	Eligibility routing.Eligibility
}

func (s *Server) CheckSessionAuthority(ctx context.Context, x *Call, token, keyID string, p *Pin) SessionAuthCheck {
	authority, ok := s.sessionAuthorityValue(ctx, x, token, keyID)
	if !ok {
		return SessionAuthCheck{Status: SessionAuthorityRevoked}
	}
	if eligibility := s.PinEligibility(p); eligibility != routing.Eligible {
		return SessionAuthCheck{Status: SessionCredentialIneligible, Eligibility: eligibility}
	}
	current := *authority
	current.EndUserDigest = x.endUserDigest
	if e := s.UnboundedPinLimits(x.AdmissionAuthority(current), p, x.Route); e != nil {
		return SessionAuthCheck{Status: SessionBudgetChanged}
	}
	return SessionAuthCheck{Status: SessionAuthorized}
}

func (s *Server) SessionAuthority(ctx context.Context, x *Call, token, keyID string, p *Pin) *fault.Error {
	check := s.CheckSessionAuthority(ctx, x, token, keyID, p)
	switch check.Status {
	case SessionAuthorized:
		return nil
	case SessionAuthorityRevoked:
		return fault.PermissionError("key_revoked", "The realtime session no longer has route authority.")
	case SessionCredentialIneligible:
		return fault.PermissionError("provider_credential_"+string(check.Eligibility), "The realtime provider credential is no longer eligible.")
	default:
		authority, _ := s.sessionAuthorityValue(ctx, x, token, keyID)
		if authority == nil {
			return fault.PermissionError("key_revoked", "The realtime session no longer has route authority.")
		}
		current := *authority
		current.EndUserDigest = x.endUserDigest
		return s.UnboundedPinLimits(x.AdmissionAuthority(current), p, x.Route)
	}
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
