package codeadapter_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/upstream"
)

func glmVendor(t *testing.T) (codeadapter.Vendor, codeadapter.Endpoint) {
	t.Helper()
	vendor, ok := codeadapter.Lookup(codemode.AdapterZAICoding)
	if !ok {
		t.Fatal("glm vendor missing")
	}
	endpoint, ok := vendor.Endpoint("v1/chat/completions")
	if !ok {
		t.Fatal("glm chat endpoint missing")
	}
	return vendor, endpoint
}

func glmBody(code, message string) []byte {
	return []byte(`{"error":{"code":"` + code + `","message":"` + message + `"}}`)
}

func TestClassifyGLMQuotaCodesAreCredentialQuotaWithTemplateReset(t *testing.T) {
	// Source: https://docs.z.ai/api-reference/api-code (fetched 2026-11-05), codes 1308, 1310, 1316-1321.
	vendor, endpoint := glmVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	stamp := reset.Format(time.RFC3339)
	cases := []struct{ code, message string }{
		{"1308", "Usage limit reached for 5 hours. Your limit will reset at " + stamp},
		{"1310", "Weekly/Monthly Limit Exhausted. Your limit will reset at " + stamp},
		{"1316", "Usage limit reached for the past 5 hours. Insufficient balance for extra usage. Resets at " + stamp + "."},
		{"1317", "Usage limit reached for the past 7 days. Insufficient balance for extra usage. Resets at " + stamp + "."},
		{"1318", "Usage limit reached for the past 5 hours. Extra usage is not available due to monthly spend limit. Resets at " + stamp + "."},
		{"1319", "Usage limit reached for the past 7 days. Extra usage is not available due to monthly spend limit. Resets at " + stamp + "."},
		{"1320", "Usage limit reached for the past 5 hours. Extra usage is not available due to monthly spend limit. Resets at " + stamp + "."},
		{"1321", "Usage limit reached for the past 7 days. Extra usage is not available due to monthly spend limit. Resets at " + stamp + "."},
	}
	for _, tc := range cases {
		got, ok := vendor.Classify(endpoint, 429, glmBody(tc.code, tc.message), http.Header{"Retry-After": {"99999"}}, now)
		if !ok || got.Class != codeadapter.CooldownQuota || got.Scope != codeadapter.ScopeCredential || !got.RetryAt.Equal(reset) || got.Envelope != "glm_"+tc.code || !got.NotExecuted {
			t.Fatalf("Classify(%s) = %+v, %v", tc.code, got, ok)
		}
	}
}

func TestClassifyGLMResetRequiresFullTemplateAndUnambiguousTimestamp(t *testing.T) {
	// Source: https://docs.z.ai/api-reference/api-code (fetched 2026-11-05), reset templates for 1308, 1310, 1316.
	vendor, endpoint := glmVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	stamp := reset.Format(time.RFC3339)
	cases := []struct {
		name    string
		code    string
		message string
		headers http.Header
		want    time.Time
	}{
		{"prose with timestamp", "1310", "Limit exhausted, try after " + stamp, http.Header{}, now.Add(time.Minute)},
		{"ambiguous timestamp without zone", "1310", "Weekly/Monthly Limit Exhausted. Your limit will reset at 2026-11-05 12:00:00", http.Header{}, now.Add(time.Minute)},
		{"ambiguous date order", "1310", "Weekly/Monthly Limit Exhausted. Your limit will reset at 11/05/2026 12:00", http.Header{}, now.Add(time.Minute)},
		{"another code template", "1316", "Weekly/Monthly Limit Exhausted. Your limit will reset at " + stamp, http.Header{}, now.Add(time.Minute)},
		{"trailing text after timestamp", "1310", "Weekly/Monthly Limit Exhausted. Your limit will reset at " + stamp + " please wait", http.Header{}, now.Add(time.Minute)},
		{"1308 without number and unit", "1308", "Usage limit reached for. Your limit will reset at " + stamp, http.Header{}, now.Add(time.Minute)},
		{"past reset falls back to single retry-after", "1310", "Weekly/Monthly Limit Exhausted. Your limit will reset at " + now.Add(-time.Hour).Format(time.RFC3339), http.Header{"Retry-After": {"300"}}, now.Add(300 * time.Second)},
		{"non matching message uses single retry-after", "1308", "usage limit reached", http.Header{"Retry-After": {"300"}}, now.Add(300 * time.Second)},
		{"multiple retry-after values ignored", "1308", "usage limit reached", http.Header{"Retry-After": {"300", "600"}}, now.Add(time.Minute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := vendor.Classify(endpoint, 429, glmBody(tc.code, tc.message), tc.headers, now)
			if !ok || got.Class != codeadapter.CooldownQuota || !got.RetryAt.Equal(tc.want) {
				t.Fatalf("Classify = %+v, %v want retry at %s", got, ok, tc.want)
			}
		})
	}
}

func TestClassifyGLMRateCapacityAndEntitlementStayShortAndApart(t *testing.T) {
	// Source: https://docs.z.ai/api-reference/api-code (fetched 2026-11-05), codes 1302, 1305, 1311.
	vendor, endpoint := glmVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		code, message string
		class         codeadapter.CooldownClass
		scope         codeadapter.CooldownScope
	}{
		{"1302", "Rate limit reached for requests", codeadapter.CooldownRate, codeadapter.ScopeCredential},
		{"1305", "The service may be temporarily overloaded, please try again later", codeadapter.CooldownCapacity, codeadapter.ScopeModel},
		{"1311", "Your current subscription plan does not yet include access to glm-5.2", codeadapter.CooldownEntitlement, codeadapter.ScopeModel},
	}
	for _, tc := range cases {
		got, ok := vendor.Classify(endpoint, 429, glmBody(tc.code, tc.message), http.Header{}, now)
		if !ok || got.Class != tc.class || got.Scope != tc.scope || !got.RetryAt.Equal(now.Add(time.Minute)) || got.Envelope != "glm_"+tc.code || got.NotExecuted {
			t.Fatalf("Classify(%s) = %+v, %v", tc.code, got, ok)
		}
		got, ok = vendor.Classify(endpoint, 429, glmBody(tc.code, tc.message), http.Header{"Retry-After": {"300"}}, now)
		if !ok || !got.RetryAt.Equal(now.Add(300*time.Second)) {
			t.Fatalf("Classify(%s) with retry-after = %+v, %v", tc.code, got, ok)
		}
	}
}

func TestClassifyGLMExpirySuppressesFallback(t *testing.T) {
	// Source: https://docs.z.ai/api-reference/api-code (fetched 2026-11-05), codes 1309, 1314.
	vendor, endpoint := glmVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ code, message string }{
		{"1309", "Your GLM Coding Plan package has expired and is temporarily unavailable. You can resume using it after renewing the subscription on the official website. https://z.ai/subscribe"},
		{"1314", "Your enterprise package has expired. Please contact your enterprise administrator."},
	} {
		got, ok := vendor.Classify(endpoint, 429, glmBody(tc.code, tc.message), http.Header{"Retry-After": {"300"}}, now)
		if !ok || !got.SuppressFallback || got.Class != "" || got.Scope != "" || !got.RetryAt.IsZero() || got.Envelope != "glm_"+tc.code || got.NotExecuted {
			t.Fatalf("Classify(%s) = %+v, %v, want suppress-fallback only", tc.code, got, ok)
		}
	}
}

func TestClassifyGLMOtherCodesStayUnclassified(t *testing.T) {
	// Source: https://docs.z.ai/api-reference/api-code (fetched 2026-11-05), codes 1113, 1313, 1315.
	vendor, endpoint := glmVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ code, message string }{
		{"1113", "Insufficient balance or no resource package. Please recharge."},
		{"1313", "Your account's current usage pattern does not comply with the Fair Usage Policy"},
		{"1315", "This API Key is limited to enterprise coding package scenarios."},
		{"9999", "unknown"},
	} {
		if got, ok := vendor.Classify(endpoint, 429, glmBody(tc.code, tc.message), http.Header{}, now); ok {
			t.Fatalf("Classify(%s) = %+v, want unclassified", tc.code, got)
		}
	}
	for name, body := range map[string][]byte{
		"numeric code":       []byte(`{"error":{"code":1308,"message":"Usage limit reached for 5 hours. Your limit will reset at 2026-11-05T12:00:00Z"}}`),
		"extra root member":  []byte(`{"error":{"code":"1308","message":"x"},"extra":1}`),
		"extra error member": []byte(`{"error":{"code":"1308","message":"x","type":"rate_limit_error"}}`),
		"missing message":    []byte(`{"error":{"code":"1308"}}`),
		"stream wrapped":     []byte(`{"type":"error","error":{"code":"1308","message":"x"}}`),
	} {
		if got, ok := vendor.Classify(endpoint, 429, body, http.Header{}, now); ok {
			t.Fatalf("Classify(%s) = %+v, want unclassified", name, got)
		}
	}
	if got, ok := vendor.Classify(endpoint, 500, glmBody("1308", "x"), http.Header{}, now); ok {
		t.Fatalf("Classify(500) = %+v, want unclassified", got)
	}
	other, found := codeadapter.Lookup(codemode.AdapterCodex)
	if !found {
		t.Fatal("codex vendor missing")
	}
	if got, ok := other.Classify(endpoint, 429, glmBody("1308", "x"), http.Header{}, now); ok {
		t.Fatalf("codex Classify(glm body) = %+v, want unclassified", got)
	}
}

func TestGLMInitialRejectionOnlyForProvenQuotaEnvelopes(t *testing.T) {
	// Source: https://docs.z.ai/api-reference/api-code (fetched 2026-11-05), code 1308 pre-generation 429 envelope; complete unary body before any content is what gateway recover() hands over.
	vendor, endpoint := glmVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	proven := glmBody("1308", "Usage limit reached for 5 hours. Your limit will reset at "+reset.Format(time.RFC3339))
	rejection, ok := vendor.InitialRejection(endpoint, 429, proven, http.Header{}, now)
	if !ok || rejection.Class != upstream.RateLimit || rejection.Envelope != "glm_1308" || !rejection.RetryAt.Equal(reset) {
		t.Fatalf("InitialRejection(proven) = %+v, %v", rejection, ok)
	}
	for name, body := range map[string][]byte{
		"extra root member": []byte(`{"error":{"code":"1308","message":"Usage limit reached for 5 hours. Your limit will reset at 2026-11-05T12:00:00Z"},"extra":1}`),
		"entitlement":       glmBody("1311", "Your current subscription plan does not yet include access to glm-5.2"),
		"rate":              glmBody("1302", "Rate limit reached for requests"),
		"expiry":            glmBody("1309", "Your GLM Coding Plan package has expired"),
	} {
		if rejection, ok := vendor.InitialRejection(endpoint, 429, body, http.Header{}, now); ok {
			t.Fatalf("InitialRejection(%s) = %+v, want unproven", name, rejection)
		}
	}
	if rejection, ok := vendor.InitialRejection(endpoint, 401, proven, http.Header{}, now); ok {
		t.Fatalf("InitialRejection(401) = %+v, want unproven", rejection)
	}
}
