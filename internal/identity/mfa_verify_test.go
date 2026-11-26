package identity

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"github.com/tyk-swe/olp/internal/access/httpkit"
)

func TestTOTPCounterWindowAndReplay(t *testing.T) {
	t.Parallel()
	const secret = "JBSWY3DPEHPK3PXP"
	const step = int64(2000)
	now := time.Unix(step*30+10, 0)
	codeAt := func(counter int64) string {
		code, err := hotp.GenerateCodeCustom(secret, uint64(counter), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if err != nil {
			t.Fatal(err)
		}
		return code
	}

	if counter, ok := totpCounter(secret, codeAt(step), now, -1); !ok || counter != step {
		t.Fatalf("current step: counter=%d ok=%v", counter, ok)
	}
	if counter, ok := totpCounter(secret, codeAt(step-1), now, -1); !ok || counter != step-1 {
		t.Fatalf("previous step: counter=%d ok=%v", counter, ok)
	}
	if counter, ok := totpCounter(secret, codeAt(step+1), now, -1); !ok || counter != step+1 {
		t.Fatalf("next step: counter=%d ok=%v", counter, ok)
	}
	if _, ok := totpCounter(secret, codeAt(step-2), now, -1); ok {
		t.Fatal("code two steps behind was accepted")
	}
	if _, ok := totpCounter(secret, codeAt(step+2), now, -1); ok {
		t.Fatal("code two steps ahead was accepted")
	}
	if _, ok := totpCounter(secret, codeAt(step), now, step); ok {
		t.Fatal("replayed current counter was accepted")
	}
	if _, ok := totpCounter(secret, codeAt(step-1), now, step); ok {
		t.Fatal("older counter after replay cursor was accepted")
	}
	if counter, ok := totpCounter(secret, codeAt(step+1), now, step); !ok || counter != step+1 {
		t.Fatalf("next step after last=%d: counter=%d ok=%v", step, counter, ok)
	}

	epoch := time.Unix(10, 0)
	if counter, ok := totpCounter(secret, codeAt(0), epoch, -1); !ok || counter != 0 {
		t.Fatalf("epoch step 0: counter=%d ok=%v", counter, ok)
	}
	if _, ok := totpCounter(secret, codeAt(0), epoch, 0); ok {
		t.Fatal("replayed epoch step 0 was accepted")
	}
	if counter, ok := totpCounter(secret, codeAt(1), epoch, 0); !ok || counter != 1 {
		t.Fatalf("epoch step 1 after 0: counter=%d ok=%v", counter, ok)
	}

	for _, bad := range []string{"", "12345", "1234567", "12x456", " 12345", "12345 "} {
		if _, ok := totpCounter(secret, bad, now, -1); ok {
			t.Fatalf("malformed code %q was accepted", bad)
		}
	}
}

func TestRecoveryCodeFormatAndNormalize(t *testing.T) {
	t.Parallel()
	raw := [16]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10}
	formatted, normalized := formatRecoveryCode(raw)
	const wantFormatted = "01234567-89ABCDEF-FEDCBA98-76543210"
	const wantNormalized = "0123456789ABCDEFFEDCBA9876543210"
	if formatted != wantFormatted {
		t.Fatalf("formatted = %q, want %q", formatted, wantFormatted)
	}
	if normalized != wantNormalized {
		t.Fatalf("normalized = %q, want %q", normalized, wantNormalized)
	}

	validCases := []string{
		wantFormatted,
		strings.ToLower(wantFormatted),
		"  " + strings.ToLower(wantFormatted) + "\n",
		wantNormalized,
		strings.ToLower(wantNormalized),
	}
	for _, input := range validCases {
		got, ok := normalizeRecoveryCode(input)
		if !ok || got != wantNormalized {
			t.Fatalf("normalizeRecoveryCode(%q) = %q, %v; want %q, true", input, got, ok, wantNormalized)
		}
	}

	invalidCases := []string{
		"",
		"   ",
		"01234567-89ABCDEF-FEDCBA98-7654321",
		"01234567-89ABCDEF-FEDCBA98-765432100",
		"01234567-89ABCDEF-FEDCBA98-7654321G",
		"ZZZZZZZZ-ZZZZZZZZ-ZZZZZZZZ-ZZZZZZZZ",
	}
	for _, input := range invalidCases {
		if got, ok := normalizeRecoveryCode(input); ok {
			t.Fatalf("normalizeRecoveryCode(%q) = %q, true; want false", input, got)
		}
	}
}

func TestWebAuthnOriginPolicy(t *testing.T) {
	t.Parallel()
	allowed := []struct {
		origin   string
		wantRPID string
	}{
		{"https://proxy.example.com", "proxy.example.com"},
		{"https://proxy.example.com:8443", "proxy.example.com"},
		{"http://localhost", "localhost"},
		{"http://localhost:8080", "localhost"},
		{"http://LOCALHOST:3000", "localhost"},
		{"http://app.localhost:8080", "app.localhost"},
		{"https://localhost:8443", "localhost"},
	}
	for _, tc := range allowed {
		w, err := webAuthnForOrigin(tc.origin)
		if err != nil {
			t.Fatalf("webAuthnForOrigin(%q) unexpected error: %v", tc.origin, err)
		}
		if w.Config.RPID != tc.wantRPID {
			t.Fatalf("webAuthnForOrigin(%q) RPID = %q, want %q", tc.origin, w.Config.RPID, tc.wantRPID)
		}
		if len(w.Config.RPOrigins) != 1 || w.Config.RPOrigins[0] != tc.origin {
			t.Fatalf("webAuthnForOrigin(%q) RPOrigins = %v, want [%q]", tc.origin, w.Config.RPOrigins, tc.origin)
		}
	}

	rejected := []string{
		"",
		"://bad-origin",
		"http://example.com",
		"http://example.com:8080",
		"http://notlocalhost:8080",
		"http://localhost.example.com",
		"http://.localhost:8080",
		"ftp://localhost",
		"https://",
		"https://127.0.0.1",
	}
	for _, origin := range rejected {
		if _, err := webAuthnForOrigin(origin); err == nil {
			t.Fatalf("webAuthnForOrigin(%q) succeeded, want error", origin)
		}
	}
}

func TestValidateMFAEnrollInput(t *testing.T) {
	t.Parallel()
	valid := []mfaEnrollInput{
		{Kind: "totp", Name: "Authenticator"},
		{Kind: "webauthn", Name: strings.Repeat("é", 100)},
	}
	for _, input := range valid {
		if err := validateMFAEnrollInput(input); err != nil {
			t.Fatalf("validateMFAEnrollInput(%+v) unexpected error: %v", input, err)
		}
	}

	invalid := []struct {
		input mfaEnrollInput
		field string
	}{
		{mfaEnrollInput{Kind: "", Name: "Key"}, "kind"},
		{mfaEnrollInput{Kind: "recovery", Name: "Key"}, "kind"},
		{mfaEnrollInput{Kind: "totp", Name: ""}, "name"},
		{mfaEnrollInput{Kind: "totp", Name: "   "}, "name"},
		{mfaEnrollInput{Kind: "webauthn", Name: strings.Repeat("a", 101)}, "name"},
	}
	for _, tc := range invalid {
		err := validateMFAEnrollInput(tc.input)
		if err == nil {
			t.Fatalf("validateMFAEnrollInput(%+v) succeeded, want error on %q", tc.input, tc.field)
		}
		problem, ok := httpkit.ProblemFor(err)
		if !ok || problem.Status != 422 || problem.Field != tc.field {
			t.Fatalf("validateMFAEnrollInput(%+v) problem = %+v, want 422 on %q", tc.input, problem, tc.field)
		}
	}
}

func TestMFAResponseJSONContracts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	t.Run("challenge preserves null public_key and empty methods array", func(t *testing.T) {
		raw, err := json.Marshal(mfaChallengeResponse{
			Challenge:          "tok",
			ExpiresAt:          now,
			Methods:            []string{},
			EnrollmentRequired: true,
			PublicKey:          nil,
			WebAuthnAvailable:  false,
		})
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if v, exists := doc["public_key"]; !exists || v != nil {
			t.Fatalf("public_key = (%v, exists=%v), want (nil, true)", v, exists)
		}
		methods, ok := doc["methods"].([]any)
		if !ok || len(methods) != 0 {
			t.Fatalf("methods = %#v, want empty JSON array", doc["methods"])
		}
	})

	t.Run("status preserves empty factors array and null last_used_at", func(t *testing.T) {
		raw, err := json.Marshal(mfaStatusResponse{
			Factors: []mfaFactorSummary{
				{ID: "f1", Kind: "totp", Name: "Phone", CreatedAt: now, LastUsedAt: nil},
			},
			RecoveryCodesRemaining: 10,
			Required:               true,
			ETag:                   "rev",
			WebAuthnAvailable:      true,
		})
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		factors := doc["factors"].([]any)
		first := factors[0].(map[string]any)
		if v, exists := first["last_used_at"]; !exists || v != nil {
			t.Fatalf("last_used_at = (%v, exists=%v), want (nil, true)", v, exists)
		}
	})

	t.Run("enrollment omits fields of the other kind", func(t *testing.T) {
		totpRaw, err := json.Marshal(mfaEnrollmentResponse{
			Kind:       "totp",
			Challenge:  "c1",
			ExpiresAt:  now,
			Secret:     "SECRET",
			OTPAuthURL: "otpauth://totp/OpenLLMProxy:u@example.com",
			QRCode:     "data:image/png;base64,AA==",
		})
		if err != nil {
			t.Fatal(err)
		}
		var totpDoc map[string]any
		if err := json.Unmarshal(totpRaw, &totpDoc); err != nil {
			t.Fatal(err)
		}
		if _, exists := totpDoc["public_key"]; exists {
			t.Fatalf("totp enrollment unexpectedly included public_key: %s", totpRaw)
		}

		webRaw, err := json.Marshal(mfaEnrollmentResponse{
			Kind:      "webauthn",
			Challenge: "c2",
			ExpiresAt: now,
			PublicKey: &protocol.CredentialCreation{},
		})
		if err != nil {
			t.Fatal(err)
		}
		var webDoc map[string]any
		if err := json.Unmarshal(webRaw, &webDoc); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"secret", "otpauth_url", "qr_code"} {
			if _, exists := webDoc[field]; exists {
				t.Fatalf("webauthn enrollment unexpectedly included %s: %s", field, webRaw)
			}
		}
		if _, exists := webDoc["public_key"]; !exists {
			t.Fatalf("webauthn enrollment missing public_key: %s", webRaw)
		}
	})

	t.Run("enrolled preserves null recovery_codes when none issued", func(t *testing.T) {
		raw, err := json.Marshal(mfaEnrolledResponse{Enrolled: true, RecoveryCodes: nil})
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if v, exists := doc["recovery_codes"]; !exists || v != nil {
			t.Fatalf("recovery_codes = (%v, exists=%v), want (nil, true)", v, exists)
		}
	})
}
