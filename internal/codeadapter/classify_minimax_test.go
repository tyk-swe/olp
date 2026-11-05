package codeadapter_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

func minimaxVendor(t *testing.T) (codeadapter.Vendor, codeadapter.Endpoint) {
	t.Helper()
	vendor, ok := codeadapter.Lookup(codemode.AdapterMiniMaxPlan)
	if !ok {
		t.Fatal("minimax vendor missing")
	}
	endpoint, ok := vendor.Endpoint("v1/chat/completions")
	if !ok {
		t.Fatal("minimax chat endpoint missing")
	}
	return vendor, endpoint
}

func minimaxBody(code, message string) []byte {
	return []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"` + message + ` (` + code + `)","http_code":"429"},"request_id":"021785229015510a2c883cf675b9804d"}`)
}

func TestClassifyMiniMaxUsageCodesAreCredentialQuota(t *testing.T) {
	// Sources: https://platform.minimax.io/docs/api-reference/errorcode (fetched 2026-11-05), codes 2056, 2067, and https://platform.minimax.io/docs/api-reference/speech-to-text (fetched 2026-11-05), OaiError envelope with the internal code in parentheses at the end of error.message. That envelope is documented only on speech-to-text plus an observed contract fixture, not proven for chat/messages inference, so replay waits for live envelope qualification and NotExecuted stays false
	vendor, endpoint := minimaxVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ code, message string }{
		{"2056", "usage limit exceeded"},
		{"2067", "usage limit reached and credit auto consumption is off"},
	} {
		got, ok := vendor.Classify(endpoint, 429, minimaxBody(tc.code, tc.message), http.Header{}, now)
		if !ok || got.Class != codeadapter.CooldownQuota || got.Scope != codeadapter.ScopeCredential || !got.RetryAt.Equal(now.Add(time.Minute)) || got.Envelope != "minimax_"+tc.code || got.NotExecuted || got.SuppressFallback {
			t.Fatalf("Classify(%s) = %+v, %v", tc.code, got, ok)
		}
		got, ok = vendor.Classify(endpoint, 429, minimaxBody(tc.code, tc.message), http.Header{"Retry-After": {"300"}}, now)
		if !ok || !got.RetryAt.Equal(now.Add(300*time.Second)) {
			t.Fatalf("Classify(%s) with retry-after = %+v, %v", tc.code, got, ok)
		}
	}
}

func TestClassifyMiniMaxRateAndEntitlementStayShortAndApart(t *testing.T) {
	// Sources: https://platform.minimax.io/docs/api-reference/errorcode (fetched 2026-11-05), codes 2061, 2062, 1002, 1039, and https://platform.minimax.io/docs/api-reference/speech-to-text (fetched 2026-11-05), Err429 example carrying code 1002 at HTTP 429.
	vendor, endpoint := minimaxVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		code, message string
		class         codeadapter.CooldownClass
		scope         codeadapter.CooldownScope
	}{
		{"2061", "your current token plan not support model", codeadapter.CooldownEntitlement, codeadapter.ScopeModel},
		{"2062", "M Plan rate limit reached", codeadapter.CooldownRate, codeadapter.ScopeCredential},
		{"1002", "rate limit, please retry later", codeadapter.CooldownRate, codeadapter.ScopeCredential},
		{"1039", "token limit", codeadapter.CooldownRate, codeadapter.ScopeCredential},
	}
	for _, tc := range cases {
		got, ok := vendor.Classify(endpoint, 429, minimaxBody(tc.code, tc.message), http.Header{"Retry-After": {"300", "600"}}, now)
		if !ok || got.Class != tc.class || got.Scope != tc.scope || !got.RetryAt.Equal(now.Add(time.Minute)) || got.Envelope != "minimax_"+tc.code || got.NotExecuted {
			t.Fatalf("Classify(%s) = %+v, %v", tc.code, got, ok)
		}
	}
}

func TestClassifyMiniMaxAmbiguous2062NeverQuotaOrReplay(t *testing.T) {
	// Source: https://platform.minimax.io/docs/api-reference/errorcode (fetched 2026-11-05), code 2062 also means no active M Plan subscription on the usage query.
	vendor, endpoint := minimaxVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	body := minimaxBody("2062", "usage limit exceeded, no active M Plan subscription")
	got, ok := vendor.Classify(endpoint, 429, body, http.Header{}, now)
	if !ok || got.Class == codeadapter.CooldownQuota || got.Class != codeadapter.CooldownRate || got.NotExecuted {
		t.Fatalf("Classify(2062) = %+v, %v, want short rate only", got, ok)
	}
	if rejection, ok := vendor.InitialRejection(endpoint, 429, body, http.Header{}, now); ok {
		t.Fatalf("InitialRejection(2062) = %+v, want unproven", rejection)
	}
}

func TestClassifyMiniMaxUnprovenShapesStayUnclassified(t *testing.T) {
	// Sources: https://platform.minimax.io/docs/api-reference/speech-to-text (fetched 2026-11-05) documents only the OaiError envelope, and https://platform.minimax.io/docs/api-reference/text-chat-openai (fetched 2026-11-05) documents base_resp only on the 200 response, so error.code and native base_resp bodies stay unclassified.
	vendor, endpoint := minimaxVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for name, body := range map[string][]byte{
		"numeric error code":   []byte(`{"error":{"code":2056,"message":"usage limit exceeded"}}`),
		"string error code":    []byte(`{"error":{"code":"2056","message":"usage limit exceeded"}}`),
		"native base_resp":     []byte(`{"base_resp":{"status_code":2056,"status_msg":"usage limit exceeded"}}`),
		"stream event":         []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded (2056)"}}`),
		"missing request id":   []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded (2056)","http_code":"429"}}`),
		"extra root member":    []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded (2056)","http_code":"429"},"request_id":"r","extra":1}`),
		"http code mismatch":   []byte(`{"type":"error","error":{"type":"insufficient_balance_error","message":"usage limit exceeded (2056)","http_code":"402"},"request_id":"r"}`),
		"code not at end":      []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"(2056) usage limit exceeded","http_code":"429"},"request_id":"r"}`),
		"no space before code": []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded(2056)","http_code":"429"},"request_id":"r"}`),
		"code only message":    []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"(2056)","http_code":"429"},"request_id":"r"}`),
		"unknown code":         minimaxBody("9999", "unknown"),
		"non digit suffix":     minimaxBody("205x", "usage limit exceeded"),
	} {
		if got, ok := vendor.Classify(endpoint, 429, body, http.Header{}, now); ok {
			t.Fatalf("Classify(%s) = %+v, want unclassified", name, got)
		}
	}
	for _, status := range []int{401, 402, 500} {
		if got, ok := vendor.Classify(endpoint, status, minimaxBody("2056", "usage limit exceeded"), http.Header{}, now); ok {
			t.Fatalf("Classify(status %d) = %+v, want unclassified", status, got)
		}
	}
	other, found := codeadapter.Lookup(codemode.AdapterCodex)
	if !found {
		t.Fatal("codex vendor missing")
	}
	if got, ok := other.Classify(endpoint, 429, minimaxBody("2056", "usage limit exceeded"), http.Header{}, now); ok {
		t.Fatalf("codex Classify(minimax body) = %+v, want unclassified", got)
	}
}

func TestMiniMaxInitialRejectionHeldUntilEnvelopeQualified(t *testing.T) {
	// Sources: https://platform.minimax.io/docs/api-reference/errorcode (fetched 2026-11-05), codes 2056, 2067, and https://platform.minimax.io/docs/api-reference/speech-to-text (fetched 2026-11-05), which documents the OaiError envelope only for speech-to-text. It is not proven for chat/messages inference endpoints, so replay waits for live envelope qualification and no MiniMax shape replays today, proven or ambiguous
	vendor, endpoint := minimaxVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ code, message string }{
		{"2056", "usage limit exceeded"},
		{"2067", "usage limit reached and credit auto consumption is off"},
		{"2062", "M Plan rate limit reached"},
	} {
		if rejection, ok := vendor.InitialRejection(endpoint, 429, minimaxBody(tc.code, tc.message), http.Header{"Retry-After": {"300"}}, now); ok {
			t.Fatalf("InitialRejection(%s) = %+v, want held pending live envelope qualification", tc.code, rejection)
		}
	}
	for name, body := range map[string][]byte{
		"extra root member": []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded (2056)","http_code":"429"},"request_id":"r","extra":1}`),
		"entitlement":       minimaxBody("2061", "your current token plan not support model"),
		"rate":              minimaxBody("1002", "rate limit, please retry later"),
		"native base_resp":  []byte(`{"base_resp":{"status_code":2056,"status_msg":"usage limit exceeded"}}`),
	} {
		if rejection, ok := vendor.InitialRejection(endpoint, 429, body, http.Header{}, now); ok {
			t.Fatalf("InitialRejection(%s) = %+v, want unproven", name, rejection)
		}
	}
	if rejection, ok := vendor.InitialRejection(endpoint, 401, minimaxBody("2056", "usage limit exceeded"), http.Header{}, now); ok {
		t.Fatalf("InitialRejection(401) = %+v, want unproven", rejection)
	}
}
