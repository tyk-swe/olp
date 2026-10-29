package consumerauth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
)

type Authenticator interface {
	Authenticate(secret string) (*access.Authority, error)
}

type DelegationAuthenticator interface {
	AuthenticateDelegation(context.Context, string, string) (*access.Authority, error)
}

type TaskCapabilityAuthenticator interface {
	AuthenticateTaskCapability(context.Context, string) (*access.Authority, error)
}

type Failure struct {
	Status  int
	Code    string
	Message string
}

func (f *Failure) Error() string { return f.Code + ": " + f.Message }

func Bearer(header string) (string, bool) {
	if len(header) < 7 || !strings.EqualFold(header[:7], "Bearer ") {
		return "", false
	}
	return strings.TrimSpace(header[7:]), true
}

func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	isTrusted := func(a netip.Addr) bool {
		return slices.ContainsFunc(trusted, func(p netip.Prefix) bool { return p.Contains(a.Unmap()) })
	}
	if !isTrusted(addr) {
		return addr.String()
	}
	chain := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for _, c := range slices.Backward(chain) {
		hop, err := netip.ParseAddr(strings.TrimSpace(c))
		if err != nil {
			break
		}
		if !isTrusted(hop) {
			return hop.String()
		}
		addr = hop
	}
	return addr.String()
}

func CheckAddress(authority access.Authority, r *http.Request, trusted []netip.Prefix) *Failure {
	if len(authority.Policy.AllowedCIDRs) != 0 && !authority.AllowsClientIP(ClientIP(r, trusted)) {
		return &Failure{Status: http.StatusForbidden, Code: "ip_not_allowed", Message: "This API key does not allow the client address."}
	}
	return nil
}

func Authenticate(auth Authenticator, r *http.Request, token, scope string, trusted []netip.Prefix, now time.Time) (access.Authority, *Failure) {
	if auth == nil || token == "" {
		return access.Authority{}, &Failure{Status: http.StatusUnauthorized, Code: "invalid_api_key", Message: "Provide an API key as a Bearer [REDACTED] in the Authorization header."}
	}
	authority, err := authenticate(auth, r, token, scope)
	switch {
	case errors.Is(err, runtime.ErrStaleAuthority):
		return access.Authority{}, &Failure{Status: http.StatusServiceUnavailable, Code: "authority_unavailable", Message: "Key authority is unavailable; retry shortly."}
	case err != nil || authority == nil:
		return access.Authority{}, &Failure{Status: http.StatusUnauthorized, Code: "invalid_api_key", Message: "Incorrect API key provided."}
	case authority.RevokedAt != nil:
		return access.Authority{}, &Failure{Status: http.StatusUnauthorized, Code: "invalid_api_key", Message: "This API key has been revoked."}
	case authority.ExpiresAt != nil && !authority.ExpiresAt.After(now):
		return access.Authority{}, &Failure{Status: http.StatusUnauthorized, Code: "invalid_api_key", Message: "This API key has expired."}
	case !slices.Contains(authority.Policy.Scopes, scope):
		return access.Authority{}, &Failure{Status: http.StatusForbidden, Code: "permission_denied", Message: "This API key does not have the " + scope + " scope."}
	}
	if failure := CheckAddress(*authority, r, trusted); failure != nil {
		return access.Authority{}, failure
	}
	return *authority, nil
}

func authenticate(auth Authenticator, r *http.Request, token, scope string) (*access.Authority, error) {
	if strings.HasPrefix(token, "olpt_") {
		capability, ok := auth.(TaskCapabilityAuthenticator)
		if !ok || (scope != "inference" && scope != "tools") {
			return nil, runtime.ErrInvalidKey
		}
		return capability.AuthenticateTaskCapability(r.Context(), token)
	}
	if !strings.HasPrefix(token, "olpd_") {
		return auth.Authenticate(token)
	}
	delegation, ok := auth.(DelegationAuthenticator)
	if !ok || (scope != "inference" && scope != "tools") {
		return nil, runtime.ErrInvalidKey
	}
	return delegation.AuthenticateDelegation(r.Context(), token, strings.TrimSpace(r.Header.Get("X-OLP-Agent-Identity")))
}
