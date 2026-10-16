//go:build integration

package integration_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/tyk-swe/olp/internal/access"
)

func mfaEnrollTOTP(t *testing.T, h *accessHarness, owner *browser) (map[string]any, []any, string, string) {
	t.Helper()
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "mfa_manage"}, nil, 204)
	enroll := h.want(owner, "POST", "/api/v1/profile/mfa/enroll", map[string]any{"kind": "totp", "name": "Phone"}, nil, 200)
	secret := enroll["secret"].(string)
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	verified := h.want(owner, "POST", "/api/v1/auth/mfa/verify", map[string]any{"challenge": enroll["challenge"], "method": "totp", "code": code}, nil, 200)
	return enroll, verified["recovery_codes"].([]any), secret, code
}
func mfaLogin(h *accessHarness, b *browser) map[string]any {
	h.t.Helper()
	return h.want(b, "POST", "/api/v1/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, 202)
}

// The consumed enrollment counter cannot also create a session.

// The refused transaction did not consume the recent proof.

// Enrollment material is sealed; only public names/IDs appear in factors.

func TestMFAPolicyPromotionAndExplicitOfflineRecovery(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	_, _, _, _ = mfaEnrollTOTP(t, h, owner)
	exported := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	if exported["require_local_mfa"] != false {
		t.Fatal(exported)
	}
	raw, _ := json.Marshal(exported)
	for _, secretState := range []string{"mfa_factors", "mfa_challenges", "recovery_codes", "otpauth"} {
		if strings.Contains(string(raw), secretState) {
			t.Fatal("MFA credentials exported")
		}
	}
	token := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "Promotion", "scopes": []string{"configure"}, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}, idem("mfa-promote-token"), 201)["secret"].(string)
	exported["require_local_mfa"] = true
	h.machineWant(token, "POST", "/api/v1/configuration/apply", map[string]any{"document": exported}, idem("mfa-promote-denied"), 403)
	h.want(owner, "POST", "/api/v1/configuration/plan", map[string]any{"document": exported}, nil, 200)
	h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": exported}, idem("mfa-promote"), 200)
	h.machineWant(token, "POST", "/api/v1/configuration/apply", map[string]any{"document": exported}, idem("mfa-promote-noop"), 200)
	result, err := access.RecoverPassword(t.Context(), h.Pool, "owner@example.com", accessPassword)
	if err != nil || result["mfa_reset"] != false {
		t.Fatal(result, err)
	}
	challenge := mfaLogin(h, &browser{})
	if challenge["enrollment_required"] != false {
		t.Fatal("ordinary password recovery cleared MFA")
	}
	result, err = access.RecoverPassword(t.Context(), h.Pool, "owner@example.com", accessPassword, true)
	if err != nil || result["mfa_reset"] != true {
		t.Fatal(result, err)
	}
	challenge = mfaLogin(h, &browser{})
	if challenge["enrollment_required"] != true {
		t.Fatal("explicit recovery bypassed policy")
	}
	var count int
	if err = h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.audit WHERE action='user.mfa_recover'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("missing recovery audit: %d %v", count, err)
	}
}
