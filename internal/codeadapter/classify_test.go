package codeadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/grokauth"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/plugin/abi"
)

func codexVendor(t *testing.T) (codeadapter.Vendor, codeadapter.Endpoint) {
	t.Helper()
	vendor, ok := codeadapter.Lookup(codemode.AdapterCodex)
	if !ok {
		t.Fatal("codex vendor missing")
	}
	endpoint, ok := vendor.Endpoint("responses")
	if !ok {
		t.Fatal("codex responses endpoint missing")
	}
	return vendor, endpoint
}

func TestClassifyCodexUsageLimitIsCredentialQuotaWithVendorReset(t *testing.T) {
	vendor, endpoint := codexVendor(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	reset := now.Add(2 * time.Hour)
	body := []byte(`{"error":{"type":"usage_limit_reached","resets_at":` + itoa(reset.Unix()) + `,"resets_in_seconds":60}}`)
	got, ok := vendor.Classify(endpoint, 429, body, http.Header{"Retry-After": {"30"}}, now)
	if !ok || got.Class != codeadapter.CooldownQuota || got.Scope != codeadapter.ScopeCredential || !got.RetryAt.Equal(reset) || got.Envelope != "codex_usage_limit_reached" || !got.NotExecuted {
		t.Fatalf("Classify = %+v, %v", got, ok)
	}
}

func TestClassifyCodexUsageLimitFallsBackToResetsInSecondsThenRetryAfterThenMinute(t *testing.T) {
	vendor, endpoint := codexVendor(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	cases := []struct {
		name    string
		body    string
		headers http.Header
		want    time.Time
	}{
		{"resets_in_seconds beats shorter retry-after", `{"error":{"type":"usage_limit_reached","resets_in_seconds":600}}`, http.Header{"Retry-After": {"30"}}, now.Add(600 * time.Second)},
		{"single retry-after beats minute", `{"error":{"type":"usage_limit_reached"}}`, http.Header{"Retry-After": {"300"}}, now.Add(300 * time.Second)},
		{"short retry-after beats minute", `{"error":{"type":"usage_limit_reached"}}`, http.Header{"Retry-After": {"7"}}, now.Add(7 * time.Second)},
		{"zero retry-after permits immediate retry", `{"error":{"type":"usage_limit_reached"}}`, http.Header{"Retry-After": {"0"}}, now},
		{"short vendor reset beats minute", `{"error":{"type":"usage_limit_reached","resets_in_seconds":12}}`, http.Header{}, now.Add(12 * time.Second)},
		{"multiple retry-after values ignored", `{"error":{"type":"usage_limit_reached"}}`, http.Header{"Retry-After": {"300", "600"}}, now.Add(time.Minute)},
		{"minute fallback", `{"error":{"type":"usage_limit_reached"}}`, http.Header{}, now.Add(time.Minute)},
		{"past reset ignored", `{"error":{"type":"usage_limit_reached","resets_at":` + itoa(now.Add(-time.Hour).Unix()) + `}}`, http.Header{}, now.Add(time.Minute)},
		{"resets_in_seconds beyond 32 bits ignored", `{"error":{"type":"usage_limit_reached","resets_in_seconds":4294967296}}`, http.Header{}, now.Add(time.Minute)},
		{"resets_at at year 10000 kept", `{"error":{"type":"usage_limit_reached","resets_at":253402300800}}`, http.Header{}, time.Unix(253402300800, 0).UTC()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := vendor.Classify(endpoint, 429, []byte(tc.body), tc.headers, now)
			if !ok || !got.RetryAt.Equal(tc.want) {
				t.Fatalf("Classify = %+v, %v want retry at %s", got, ok, tc.want)
			}
		})
	}
}

func TestClassifyCodexCapacityIsShortModelCooldownOnlyForExactCodes(t *testing.T) {
	vendor, endpoint := codexVendor(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	for _, body := range []string{
		`{"error":{"type":"rate_limit_error","code":"turn_rate_limit_exceeded"}}`,
		`{"error":{"type":"turn_rate_limit_exceeded"}}`,
		`{"error":{"type":"rate_limit_error","code":"model_at_capacity"}}`,
		`{"error":{"type":"rate_limit_error","code":"model_is_at_capacity"}}`,
	} {
		got, ok := vendor.Classify(endpoint, 429, []byte(body), http.Header{}, now)
		if !ok || got.Class != codeadapter.CooldownCapacity || got.Scope != codeadapter.ScopeModel || !got.RetryAt.Equal(now.Add(time.Minute)) || got.NotExecuted {
			t.Fatalf("Classify(%s) = %+v, %v", body, got, ok)
		}
	}
	got, ok := vendor.Classify(endpoint, 429, []byte(`{"error":{"type":"rate_limit_error","code":"turn_rate_limit_exceeded"}}`), http.Header{"Retry-After": {"120"}}, now)
	if !ok || !got.RetryAt.Equal(now.Add(120*time.Second)) {
		t.Fatalf("capacity retry-after = %+v, %v", got, ok)
	}
	for _, body := range []string{
		`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"The model is at capacity"}}`,
		`{"error":{"type":"rate_limit_error","message":"model is at capacity, try again"}}`,
		`{"error":{"type":"usage_limit_reached"}}`,
	} {
		if got, ok := vendor.Classify(endpoint, 500, []byte(body), http.Header{}, now); ok {
			t.Fatalf("Classify(%s, 500) = %+v, want unclassified", body, got)
		}
	}
	if got, ok := vendor.Classify(endpoint, 429, []byte(`{"error":{"type":"rate_limit_error","message":"model is at capacity, try again"}}`), http.Header{}, now); ok {
		t.Fatalf("prose capacity = %+v, want unclassified", got)
	}
}

func TestClassifyStreamWrappedUsageLimitCoolsButIsNotProvenNotExecuted(t *testing.T) {
	vendor, endpoint := codexVendor(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	body := []byte(`{"type":"error","error":{"type":"usage_limit_reached","resets_in_seconds":900}}`)
	got, ok := vendor.Classify(endpoint, 429, body, http.Header{}, now)
	if !ok || got.Class != codeadapter.CooldownQuota || got.NotExecuted || !got.RetryAt.Equal(now.Add(900*time.Second)) {
		t.Fatalf("Classify = %+v, %v", got, ok)
	}
	if rejection, ok := vendor.InitialRejection(endpoint, 429, body, http.Header{}, now); ok {
		t.Fatalf("InitialRejection(stream body) = %+v, want unproven", rejection)
	}
}

func TestInitialRejectionKeepsCodexEnvelopesAndClasses(t *testing.T) {
	vendor, endpoint := codexVendor(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	quota, ok := vendor.InitialRejection(endpoint, 429, []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":600}}`), http.Header{}, now)
	if !ok || quota.Class != upstream.RateLimit || quota.Envelope != "codex_usage_limit_reached" || !quota.RetryAt.Equal(now.Add(600*time.Second)) {
		t.Fatalf("quota rejection = %+v, %v", quota, ok)
	}
	credential, ok := vendor.InitialRejection(endpoint, 401, []byte(`{"error":{"type":"authentication_error","code":"invalid_api_key"}}`), http.Header{}, now)
	if !ok || credential.Class != upstream.Credential || credential.Envelope != "codex_invalid_api_key" || !credential.RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("credential rejection = %+v, %v", credential, ok)
	}
	if rejection, ok := vendor.InitialRejection(endpoint, 429, []byte(`{"error":{"type":"rate_limit_error","code":"turn_rate_limit_exceeded"}}`), http.Header{}, now); ok {
		t.Fatalf("capacity rejection = %+v, want no replay proof", rejection)
	}
	other, found := codeadapter.Lookup(codemode.AdapterOpenCodeGo)
	if !found {
		t.Fatal("opencode vendor missing")
	}
	if got, ok := other.Classify(codeadapter.Endpoint{}, 429, []byte(`{"error":{"type":"usage_limit_reached"}}`), http.Header{}, now); ok {
		t.Fatalf("non-codex Classify = %+v, want unclassified this ticket", got)
	}
	if got, ok := vendor.Classify(endpoint, 429, []byte(`{"error":{"type":"usage_limit_reached"},"extra":1}`), http.Header{}, now); !ok || got.NotExecuted {
		t.Fatalf("extra-member Classify = %+v, %v want classified without proof", got, ok)
	}
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

func TestUnqualifiedQuotaEnvelopesDoNotAuthorizeReplay(t *testing.T) {
	for _, tc := range []struct {
		adapter codemode.Adapter
		body    string
	}{
		{codemode.AdapterZAICoding, `{"error":{"code":"1308","message":"usage limit reached"}}`},
		{codemode.AdapterKimiCode, `{"error":{"message":"quota exceeded","type":"rate_limit_error","code":"exceeded_current_quota_error"}}`},
		{codemode.AdapterQwenTokenPlan, `{"error":{"message":"Your token-plan quota has been exhausted.","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`},
	} {
		vendor, ok := codeadapter.Lookup(tc.adapter)
		if !ok {
			t.Fatalf("adapter %s unavailable", tc.adapter)
		}
		endpoint, ok := vendor.Endpoint("v1/chat/completions")
		if !ok {
			t.Fatalf("adapter %s chat endpoint unavailable", tc.adapter)
		}
		now := time.Unix(1_800_000_000, 0).UTC()
		classification, ok := vendor.Classify(endpoint, 429, []byte(tc.body), http.Header{}, now)
		if !ok || classification.Class != codeadapter.CooldownQuota || classification.NotExecuted {
			t.Errorf("%s classification = %+v, %v", tc.adapter, classification, ok)
		}
		if rejection, ok := vendor.InitialRejection(endpoint, 429, []byte(tc.body), http.Header{}, now); ok {
			t.Errorf("%s unexpectedly authorizes replay: %+v", tc.adapter, rejection)
		}
	}
}

func TestGrokDeviceGrantUsesAuthenticatedSubject(t *testing.T) {
	adapter := grokauth.Adapter{Fetch: func(_ context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
		switch request.URL {
		case grokauth.Issuer + "/oauth2/device/code":
			return abi.HTTPResponse{Status: 200, Body: []byte(`{"device_code":"device-secret","user_code":"ABCD","verification_uri":"https://auth.x.ai/device","expires_in":600,"interval":5}`)}, nil
		case grokauth.Issuer + "/oauth2/token":
			return abi.HTTPResponse{Status: 200, Body: []byte(`{"access_token":"opaque-access","refresh_token":"refresh-secret","token_type":"Bearer","expires_in":3600}`)}, nil
		case grokauth.Issuer + "/oauth2/userinfo":
			if request.Method != http.MethodGet || http.Header(request.Header).Get("Authorization") != "Bearer opaque-access" {
				t.Fatal("identity was not authenticated with the new access token")
			}
			return abi.HTTPResponse{Status: 200, Body: []byte(`{"sub":"verified-subject","email":"display-only@example.invalid"}`)}, nil
		default:
			t.Fatalf("unexpected request %s", request.URL)
			return abi.HTTPResponse{}, nil
		}
	}}
	started, err := adapter.StartGrant(t.Context(), abi.GrantStart{Profile: grokauth.ProfileID})
	if err != nil || started.Device == nil || started.Device.UserCode != "ABCD" {
		t.Fatalf("start = %+v, %v", started, err)
	}
	grant, err := adapter.PollGrant(t.Context(), abi.GrantPoll{Profile: grokauth.ProfileID, Session: started.Session})
	if err != nil || grant.Principal != grokauth.Principal("verified-subject") || grant.Facts["subject"] != "verified-subject" || grant.AccessToken != "opaque-access" || grant.RefreshToken != "refresh-secret" || grant.ExpiresIn != 3600 {
		t.Fatalf("grant = %+v, %v", grant, err)
	}
	encoded, _ := json.Marshal(grant.Facts)
	if string(encoded) != `{"subject":"verified-subject"}` {
		t.Fatalf("unexpected authority facts %s", encoded)
	}
}
