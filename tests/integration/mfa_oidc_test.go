//go:build integration && oidctest

package integration_test

import (
	"github.com/pquerna/otp/totp"
	"testing"
	"time"
)

func TestLinkedOIDCDoesNotReplaceEnrolledMFAForFactorManagement(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	issuer := newTestIssuer(t)
	h.want(owner, "PUT", "/api/v1/oidc/configuration", map[string]any{"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration", "client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true}, nil, 200)
	claims := map[string]any{"sub": "owner", "email": "owner@example.com"}
	authorize := func(path string, input any, want int) {
		t.Helper()
		url := h.want(owner, "POST", path, input, nil, 200)["authorization_url"].(string)
		h.want(owner, "GET", issuer.callback(t, url, claims), nil, nil, want)
	}
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_link"}, nil, 204)
	authorize("/api/v1/oidc/link", nil, 303)
	// A freshly verified linked identity may authorize the initial enrollment.
	authorize("/api/v1/oidc/reauthenticate", map[string]any{"purpose": "mfa_manage"}, 303)
	enroll := h.want(owner, "POST", "/api/v1/profile/mfa/enroll", map[string]any{"kind": "totp", "name": "Authenticator"}, nil, 200)
	code, err := totp.GenerateCode(enroll["secret"].(string), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h.want(owner, "POST", "/api/v1/auth/mfa/verify", map[string]any{"challenge": enroll["challenge"], "method": "totp", "code": code}, nil, 200)
	authorize("/api/v1/oidc/reauthenticate", map[string]any{"purpose": "mfa_manage"}, 403)
	status := h.want(owner, "GET", "/api/v1/profile/mfa", nil, nil, 200)
	h.want(owner, "POST", "/api/v1/profile/mfa/recovery-codes", map[string]any{}, etagHeader(status), 428)
}
