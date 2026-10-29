package consumerauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
)

type fakeAuthenticator struct {
	authority *access.Authority
	err       error
}

type delegationAuthenticator struct {
	authority *access.Authority
	err       error
}

func (a delegationAuthenticator) Authenticate(string) (*access.Authority, error) {
	return nil, runtime.ErrInvalidKey
}

func (a delegationAuthenticator) AuthenticateDelegation(_ context.Context, token, identity string) (*access.Authority, error) {
	if token != "olpd_test" || identity != "agent-assertion" {
		return nil, runtime.ErrInvalidKey
	}
	return a.authority, a.err
}

func TestAuthenticateDelegationRequiresAgentIdentityAndPermittedScope(t *testing.T) {
	now := time.Now()
	authority := &access.Authority{ID: "delegating-key", Policy: access.KeyPolicy{Scopes: []string{"inference", "tools"}, AllowedCIDRs: []string{"10.0.0.0/8"}}}
	authenticator := delegationAuthenticator{authority: authority}
	request := func(identity, addr string) *http.Request {
		return &http.Request{Header: http.Header{"X-Olp-Agent-Identity": []string{identity}}, RemoteAddr: addr}
	}
	for _, scope := range []string{"inference", "tools"} {
		t.Run(scope, func(t *testing.T) {
			got, failure := Authenticate(authenticator, request(" agent-assertion ", "10.1.2.3:1234"), "olpd_test", scope, nil, now)
			if failure != nil {
				t.Fatalf("permitted delegated request refused: %v", failure)
			}
			if got.ID != authority.ID {
				t.Fatalf("unexpected delegated authority: %v", got)
			}
		})
	}
	for _, identity := range []string{"", "another-agent"} {
		if _, failure := Authenticate(authenticator, request(identity, "10.1.2.3:1234"), "olpd_test", "inference", nil, now); failure == nil || failure.Status != http.StatusUnauthorized {
			t.Fatalf("incorrect agent identity %q accepted: %v", identity, failure)
		}
	}
	if _, failure := Authenticate(authenticator, request("agent-assertion", "10.1.2.3:1234"), "olpd_test", "admin", nil, now); failure == nil || failure.Status != http.StatusUnauthorized {
		t.Fatalf("delegation accepted for unsupported scope: %v", failure)
	}
	if _, failure := Authenticate(fakeAuthenticator{authority: authority}, request("agent-assertion", "10.1.2.3:1234"), "olpd_test", "inference", nil, now); failure == nil || failure.Status != http.StatusUnauthorized {
		t.Fatalf("delegation accepted without a delegation authenticator: %v", failure)
	}
	if _, failure := Authenticate(authenticator, request("agent-assertion", "192.0.2.1:1234"), "olpd_test", "inference", nil, now); failure == nil || failure.Code != "ip_not_allowed" {
		t.Fatalf("delegation bypassed the caller address restriction: %v", failure)
	}
	for _, test := range []struct {
		name      string
		authority *access.Authority
	}{
		{name: "revoked", authority: &access.Authority{RevokedAt: &now, Policy: access.KeyPolicy{Scopes: []string{"inference"}}}},
		{name: "expired", authority: &access.Authority{ExpiresAt: &now, Policy: access.KeyPolicy{Scopes: []string{"inference"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, failure := Authenticate(delegationAuthenticator{authority: test.authority}, request("agent-assertion", "10.1.2.3:1234"), "olpd_test", "inference", nil, now); failure == nil || failure.Status != http.StatusUnauthorized {
				t.Fatalf("%s delegated authority accepted: %v", test.name, failure)
			}
		})
	}
	if _, failure := Authenticate(delegationAuthenticator{err: runtime.ErrStaleAuthority}, request("agent-assertion", "10.1.2.3:1234"), "olpd_test", "tools", nil, now); failure == nil || failure.Status != http.StatusServiceUnavailable {
		t.Fatalf("stale delegated authority did not fail closed: %v", failure)
	}
}

func (f fakeAuthenticator) Authenticate(secret string) (*access.Authority, error) {
	if secret != "good" {
		return nil, errors.New("invalid key")
	}
	return f.authority, f.err
}

func consumerAuthority() *access.Authority {
	return &access.Authority{ID: "key-one", Policy: access.KeyPolicy{Scopes: []string{"tools"}, AllowedCIDRs: []string{"10.0.0.0/8"}}}
}

func consumerRequest(remoteAddr, forwardedFor string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/mcp/tools", nil)
	r.RemoteAddr = remoteAddr
	if forwardedFor != "" {
		r.Header.Set("X-Forwarded-For", forwardedFor)
	}
	return r
}

func TestAuthenticateEnforcesScopeAndAllowedCIDRs(t *testing.T) {
	now := time.Now()
	auth := fakeAuthenticator{authority: consumerAuthority()}
	if _, failure := Authenticate(auth, consumerRequest("10.1.2.3:1234", ""), "good", "tools", nil, now); failure != nil {
		t.Fatalf("allowed client refused: %v", failure)
	}
	if _, failure := Authenticate(auth, consumerRequest("192.0.2.3:1234", "10.1.2.3"), "good", "tools", nil, now); failure == nil || failure.Code != "ip_not_allowed" || failure.Status != http.StatusForbidden {
		t.Fatalf("forwarded address from an untrusted proxy was honoured: %v", failure)
	}
	trusted := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	if _, failure := Authenticate(auth, consumerRequest("192.0.2.3:1234", "10.1.2.3"), "good", "tools", trusted, now); failure != nil {
		t.Fatalf("allowed address behind a trusted proxy refused: %v", failure)
	}
	if _, failure := Authenticate(auth, consumerRequest("192.0.2.3:1234", "203.0.113.3"), "good", "tools", trusted, now); failure == nil || failure.Code != "ip_not_allowed" {
		t.Fatalf("disallowed forwarded address accepted: %v", failure)
	}
	if _, failure := Authenticate(auth, consumerRequest("10.1.2.3:1234", ""), "good", "inference", nil, now); failure == nil || failure.Code != "permission_denied" {
		t.Fatalf("missing scope accepted: %v", failure)
	}
	if _, failure := Authenticate(auth, consumerRequest("10.1.2.3:1234", ""), "bad", "tools", nil, now); failure == nil || failure.Status != http.StatusUnauthorized {
		t.Fatalf("bad key accepted: %v", failure)
	}
}

func TestAuthenticateFailsClosedOnStaleAndInactiveAuthority(t *testing.T) {
	now := time.Now()
	stale := fakeAuthenticator{authority: consumerAuthority(), err: runtime.ErrStaleAuthority}
	if _, failure := Authenticate(stale, consumerRequest("10.1.2.3:1234", ""), "good", "tools", nil, now); failure == nil || failure.Status != http.StatusServiceUnavailable {
		t.Fatalf("stale authority admitted a caller: %v", failure)
	}
	revoked := consumerAuthority()
	revoked.RevokedAt = &now
	if _, failure := Authenticate(fakeAuthenticator{authority: revoked}, consumerRequest("10.1.2.3:1234", ""), "good", "tools", nil, now); failure == nil || failure.Status != http.StatusUnauthorized {
		t.Fatalf("revoked key admitted: %v", failure)
	}
	expired := consumerAuthority()
	expired.ExpiresAt = &now
	if _, failure := Authenticate(fakeAuthenticator{authority: expired}, consumerRequest("10.1.2.3:1234", ""), "good", "tools", nil, now.Add(time.Minute)); failure == nil || failure.Status != http.StatusUnauthorized {
		t.Fatalf("expired key admitted: %v", failure)
	}
}
