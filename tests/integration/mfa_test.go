//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/tyk-swe/olp/internal/access"
)

func mfaEnrollTOTP(t *testing.T, h *accessHarness, owner *browser) (map[string]any, []any, string) {
	t.Helper()
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "mfa_manage"}, nil, 204)
	enroll := h.want(owner, "POST", "/api/v1/profile/mfa/enroll", map[string]any{"kind": "totp", "name": "Phone"}, nil, 200)
	secret := enroll["secret"].(string)
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	verified := h.want(owner, "POST", "/api/v1/auth/mfa/verify", map[string]any{"challenge": enroll["challenge"], "method": "totp", "code": code}, nil, 200)
	return enroll, verified["recovery_codes"].([]any), secret
}
func mfaLogin(h *accessHarness, b *browser) map[string]any {
	h.t.Helper()
	return h.want(b, "POST", "/api/v1/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, 202)
}
func TestLocalMFAProtectsSessionsAndOneUseRecovery(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	enroll, codes, secret := mfaEnrollTOTP(t, h, owner)
	status := h.want(owner, "GET", "/api/v1/profile/mfa", nil, nil, 200)
	if len(status["factors"].([]any)) != 1 || len(codes) != 10 {
		t.Fatal(status)
	}
	for _, table := range []string{"mfa_factors", "mfa_challenges", "mfa_recovery_codes", "audit"} {
		var leaked bool
		if err := h.Pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM olp."+table+" x WHERE to_jsonb(x)::text LIKE '%'||$1||'%' OR to_jsonb(x)::text LIKE '%'||$2||'%')", secret, strings.ReplaceAll(codes[0].(string), "-", "")).Scan(&leaked); err != nil || leaked {
			t.Fatalf("secret in %s: %v %v", table, leaked, err)
		}
	}
	h.want(owner, "POST", "/api/v1/auth/mfa/verify", map[string]any{"challenge": enroll["challenge"], "method": "totp", "code": "000000"}, nil, 401)
	guest := &browser{}
	flow := mfaLogin(h, guest)
	h.want(guest, "GET", "/api/v1/profile", nil, nil, 401)
	// The consumed enrollment counter cannot also create a session.
	code, _ := totp.GenerateCode(secret, time.Now())
	h.want(guest, "POST", "/api/v1/auth/mfa/verify", map[string]any{"challenge": flow["challenge"], "method": "totp", "code": code}, nil, 401)
	result := h.want(guest, "POST", "/api/v1/auth/mfa/verify", map[string]any{"challenge": flow["challenge"], "method": "recovery", "code": codes[0]}, nil, 201)
	if result["user"] == nil {
		t.Fatal(result)
	}
	h.want(guest, "GET", "/api/v1/profile", nil, nil, 200)
	again := &browser{}
	challenge := mfaLogin(h, again)
	h.want(again, "POST", "/api/v1/auth/mfa/verify", map[string]any{"challenge": challenge["challenge"], "method": "recovery", "code": codes[0]}, nil, 401)
	remaining := h.want(guest, "GET", "/api/v1/profile/mfa", nil, nil, 200)
	if remaining["recovery_codes_remaining"] != float64(9) {
		t.Fatal(remaining)
	}
}
func TestMFAManagementNeedsFreshProofAndRequiredPolicyProtectsLastFactor(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	_, codes, _ := mfaEnrollTOTP(t, h, owner)
	status := h.want(owner, "GET", "/api/v1/profile/mfa", nil, nil, 200)
	factor := status["factors"].([]any)[0].(map[string]any)["id"].(string)
	h.want(owner, "DELETE", "/api/v1/profile/mfa/factors/"+factor, nil, etagHeader(status), 428)
	recent := h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "mfa_manage"}, nil, 202)
	h.want(owner, "POST", "/api/v1/profile/mfa/recovery-codes", map[string]any{}, etagHeader(status), 428)
	h.want(owner, "POST", "/api/v1/auth/mfa/verify", map[string]any{"challenge": recent["challenge"], "method": "recovery", "code": codes[0]}, nil, 204)
	rotated := h.want(owner, "POST", "/api/v1/profile/mfa/recovery-codes", map[string]any{}, etagHeader(status), 200)["recovery_codes"].([]any)
	policy := h.want(owner, "GET", "/api/v1/settings/auth.mfa_required", nil, nil, 200)
	h.want(owner, "PUT", "/api/v1/settings/auth.mfa_required", map[string]any{"value": "true"}, etagHeader(policy), 200)
	recent = h.want(owner, "POST", "/api/v1/profile/mfa/challenge", map[string]any{}, nil, 202)
	h.want(owner, "POST", "/api/v1/auth/mfa/verify", map[string]any{"challenge": recent["challenge"], "method": "recovery", "code": rotated[0]}, nil, 204)
	status = h.want(owner, "GET", "/api/v1/profile/mfa", nil, nil, 200)
	h.want(owner, "DELETE", "/api/v1/profile/mfa/factors/"+factor, nil, etagHeader(status), 409)
	policy = h.want(owner, "GET", "/api/v1/settings/auth.mfa_required", nil, nil, 200)
	h.want(owner, "PUT", "/api/v1/settings/auth.mfa_required", map[string]any{"value": "false"}, etagHeader(policy), 200)
	// The refused transaction did not consume the recent proof.
	h.want(owner, "DELETE", "/api/v1/profile/mfa/factors/"+factor, nil, etagHeader(status), 204)
	status = h.want(owner, "GET", "/api/v1/profile/mfa", nil, nil, 200)
	if len(status["factors"].([]any)) != 0 || status["recovery_codes_remaining"] != float64(0) {
		t.Fatal(status)
	}
	h.want(&browser{}, "POST", "/api/v1/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, 201)
}
func TestRequiredMFAEnrollmentNeverCreatesAPasswordOnlySession(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	policy := h.want(owner, "GET", "/api/v1/settings/auth.mfa_required", nil, nil, 200)
	h.want(owner, "PUT", "/api/v1/settings/auth.mfa_required", map[string]any{"value": "true"}, etagHeader(policy), 200)
	h.want(owner, "GET", "/api/v1/profile", nil, nil, 401)
	guest := &browser{}
	challenge := mfaLogin(h, guest)
	if challenge["enrollment_required"] != true {
		t.Fatal(challenge)
	}
	enrolled := h.want(guest, "POST", "/api/v1/auth/mfa/enroll", map[string]any{"challenge": challenge["challenge"], "kind": "totp", "name": "Required authenticator"}, nil, 200)
	h.want(guest, "GET", "/api/v1/profile", nil, nil, 401)
	code, _ := totp.GenerateCode(enrolled["secret"].(string), time.Now())
	result := h.want(guest, "POST", "/api/v1/auth/mfa/verify", map[string]any{"challenge": enrolled["challenge"], "method": "totp", "code": code}, nil, 201)
	if result["session"] == nil || len(result["recovery_codes"].([]any)) != 10 {
		t.Fatal(result)
	}
	h.want(guest, "GET", "/api/v1/profile", nil, nil, 200)
	// Enrollment material is sealed; only public names/IDs appear in factors.
	raw, _ := json.Marshal(h.want(guest, "GET", "/api/v1/profile/mfa", nil, nil, 200))
	if strings.Contains(string(raw), enrolled["secret"].(string)) {
		t.Fatal("seed exposed")
	}
	if err := h.Pool.QueryRow(t.Context(), "SELECT true FROM olp.sessions WHERE user_id=(SELECT id FROM olp.users WHERE email='owner@example.com') AND mfa_verified").Scan(new(bool)); err != nil {
		t.Fatal(err)
	}
}

func TestMFAPolicyPromotionAndExplicitOfflineRecovery(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	_, _, _ = mfaEnrollTOTP(t, h, owner)
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

func TestPendingMFAFollowsCurrentLocalSignInPolicy(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	_, codes, _ := mfaEnrollTOTP(t, h, owner)
	guest := &browser{}
	flow := mfaLogin(h, guest)
	if _, err := h.Pool.Exec(context.Background(), "UPDATE olp.settings SET value='false' WHERE key='auth.local_login_enabled'"); err != nil {
		t.Fatal(err)
	}
	h.want(guest, "POST", "/api/v1/auth/mfa/verify", map[string]any{"challenge": flow["challenge"], "method": "recovery", "code": codes[0]}, nil, 401)
	h.want(guest, "GET", "/api/v1/profile", nil, nil, 401)
	status := h.want(owner, "GET", "/api/v1/profile/mfa", nil, nil, 200)
	if status["recovery_codes_remaining"] != float64(10) {
		t.Fatal("Rejected login consumed a recovery code")
	}
}

func TestMFAProofsAreSingleUseAcrossControlReplicas(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	_, codes, secret := mfaEnrollTOTP(t, h, owner)
	replica := newAccessHarnessOn(t, h.Pool, h.DBURL)
	code, err := totp.GenerateCode(secret, time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, proof := range []struct{ method, value string }{{"totp", code}, {"recovery", codes[0].(string)}} {
		t.Run(proof.method, func(t *testing.T) {
			clients := []*browser{{}, {}}
			servers := []*accessHarness{h, replica}
			flows := []map[string]any{mfaLogin(h, clients[0]), mfaLogin(replica, clients[1])}
			results := make(chan int, 2)
			for i := range servers {
				go func(i int) {
					status, _, _ := servers[i].request(clients[i], "POST", "/api/v1/auth/mfa/verify", map[string]any{"challenge": flows[i]["challenge"], "method": proof.method, "code": proof.value}, nil)
					results <- status
				}(i)
			}
			first, second := <-results, <-results
			if !((first == 201 && second == 401) || (first == 401 && second == 201)) {
				t.Fatalf("one proof issued multiple sessions or no session: %d %d", first, second)
			}
		})
	}
}

func TestMalformedMFAPolicyCannotAuthorizePasswordOnlySessions(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	if _, err := h.Pool.Exec(context.Background(), "UPDATE olp.settings SET value='invalid' WHERE key='auth.mfa_required'"); err != nil {
		t.Fatal(err)
	}
	h.want(owner, "GET", "/api/v1/profile", nil, nil, 401)
	h.want(&browser{}, "POST", "/api/v1/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, 503)
}
