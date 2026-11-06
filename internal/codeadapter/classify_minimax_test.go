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

func minimaxBaseRespBody(code, message string) []byte {
	return []byte(`{"base_resp":{"status_code":` + code + `,"status_msg":"` + message + `"}}`)
}

func minimaxErrorCodeBody(code, message string) []byte {
	return []byte(`{"error":{"message":"` + message + `","type":"rate_limit_error","code":` + code + `,"http_code":429}}`)
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

func TestClassifyMiniMaxDeclaredEnvelopesCoverEveryCode(t *testing.T) {
	// Sources: MiniMax-AI/cli@38aba5f (fetched 2026-11-05), src/errors/api.ts ApiErrorBody declares base_resp{status_code number, status_msg string} and error{message, type string, code number, http_code string or number} for every command, chat included, and https://platform.minimax.io/docs/api-reference/errorcode (fetched 2026-11-05) for the code meanings. The vendor mapper reads these envelopes but classifies mostly by HTTP status, so the code to class map stays the documented one already used for the parenthesized envelope
	vendor, endpoint := minimaxVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		code, message string
		class         codeadapter.CooldownClass
		scope         codeadapter.CooldownScope
	}{
		{"2056", "usage limit exceeded", codeadapter.CooldownQuota, codeadapter.ScopeCredential},
		{"2067", "usage limit reached and credit auto consumption is off", codeadapter.CooldownQuota, codeadapter.ScopeCredential},
		{"2061", "your current token plan not support model", codeadapter.CooldownEntitlement, codeadapter.ScopeModel},
		{"2062", "M Plan rate limit reached", codeadapter.CooldownRate, codeadapter.ScopeCredential},
		{"1002", "rate limit, please retry later", codeadapter.CooldownRate, codeadapter.ScopeCredential},
		{"1039", "token limit", codeadapter.CooldownRate, codeadapter.ScopeCredential},
	}
	for _, tc := range cases {
		for name, body := range map[string][]byte{
			"base_resp":  minimaxBaseRespBody(tc.code, tc.message),
			"error code": minimaxErrorCodeBody(tc.code, tc.message),
		} {
			got, ok := vendor.Classify(endpoint, 429, body, http.Header{}, now)
			if !ok || got.Class != tc.class || got.Scope != tc.scope || !got.RetryAt.Equal(now.Add(time.Minute)) || got.Envelope != "minimax_"+tc.code || got.NotExecuted || got.SuppressFallback {
				t.Fatalf("Classify(%s %s) = %+v, %v", name, tc.code, got, ok)
			}
		}
	}
}

func TestClassifyMiniMaxDeclaredFoldingPrefersBaseResp(t *testing.T) {
	// Source: MiniMax-AI/cli@38aba5f (fetched 2026-11-05), src/errors/api.ts mapApiError folds the business code as base_resp.status_code first with a truthy fallback, so a status_code of 0 falls through to error.code
	vendor, endpoint := minimaxVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	conflicting := []byte(`{"base_resp":{"status_code":2061,"status_msg":"your current token plan not support model"},"error":{"message":"usage limit exceeded","type":"rate_limit_error","code":2056,"http_code":"429"}}`)
	got, ok := vendor.Classify(endpoint, 429, conflicting, http.Header{}, now)
	if !ok || got.Class != codeadapter.CooldownEntitlement || got.Scope != codeadapter.ScopeModel || got.Envelope != "minimax_2061" || got.NotExecuted {
		t.Fatalf("Classify(base_resp wins) = %+v, %v", got, ok)
	}
	zeroBase := []byte(`{"base_resp":{"status_code":0,"status_msg":"success"},"error":{"message":"M Plan rate limit reached","type":"rate_limit_error","code":2062,"http_code":"429"}}`)
	got, ok = vendor.Classify(endpoint, 429, zeroBase, http.Header{}, now)
	if !ok || got.Class != codeadapter.CooldownRate || got.Scope != codeadapter.ScopeCredential || got.Envelope != "minimax_2062" || got.NotExecuted {
		t.Fatalf("Classify(status_code 0 falls through) = %+v, %v", got, ok)
	}
	for name, body := range map[string][]byte{
		"base_resp zero alone": minimaxBaseRespBody("0", "success"),
		"error code zero":      minimaxErrorCodeBody("0", "success"),
		"both zero":            []byte(`{"base_resp":{"status_code":0,"status_msg":"success"},"error":{"message":"success","type":"rate_limit_error","code":0}}`),
	} {
		if got, ok := vendor.Classify(endpoint, 429, body, http.Header{}, now); ok {
			t.Fatalf("Classify(%s) = %+v, want unclassified", name, got)
		}
	}
	if got, ok := vendor.Classify(endpoint, 200, minimaxBaseRespBody("2056", "usage limit exceeded"), http.Header{}, now); ok {
		t.Fatalf("Classify(200 base_resp) = %+v, want unclassified until the observers surface 200 rejection bodies", got)
	}
}

func TestClassifyMiniMaxDeclaredFoldingPerEnvelope(t *testing.T) {
	// Source: MiniMax-AI/cli@38aba5f (fetched 2026-11-05), src/errors/api.ts ApiErrorBody declares every base_resp and error subfield optional and mapApiError folds the code as base_resp.status_code then error.code, so one malformed envelope does not veto a well typed code from the other and a declared JS number may arrive as an integral float lexeme
	vendor, endpoint := minimaxVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name, envelope string
		body           []byte
		class          codeadapter.CooldownClass
		scope          codeadapter.CooldownScope
	}{
		{"status code only", "minimax_2056", []byte(`{"base_resp":{"status_code":2056}}`), codeadapter.CooldownQuota, codeadapter.ScopeCredential},
		{"status msg only falls through", "minimax_2062", []byte(`{"base_resp":{"status_msg":"success"},"error":{"message":"M Plan rate limit reached","type":"rate_limit_error","code":2062}}`), codeadapter.CooldownRate, codeadapter.ScopeCredential},
		{"malformed error, valid base_resp", "minimax_2056", []byte(`{"base_resp":{"status_code":2056,"status_msg":"usage limit exceeded"},"error":{"message":"M Plan rate limit reached","code":"2062"}}`), codeadapter.CooldownQuota, codeadapter.ScopeCredential},
		{"error not object, valid base_resp", "minimax_2056", []byte(`{"base_resp":{"status_code":2056},"error":"broken"}`), codeadapter.CooldownQuota, codeadapter.ScopeCredential},
		{"malformed base_resp, valid error code", "minimax_2062", []byte(`{"base_resp":{"status_code":"2056","status_msg":"usage limit exceeded"},"error":{"message":"M Plan rate limit reached","type":"rate_limit_error","code":2062}}`), codeadapter.CooldownRate, codeadapter.ScopeCredential},
		{"base_resp not object, valid error code", "minimax_2062", []byte(`{"base_resp":"broken","error":{"message":"M Plan rate limit reached","code":2062}}`), codeadapter.CooldownRate, codeadapter.ScopeCredential},
		{"integral float status code", "minimax_2056", []byte(`{"base_resp":{"status_code":2056.0}}`), codeadapter.CooldownQuota, codeadapter.ScopeCredential},
		{"integral exponent error code", "minimax_2056", []byte(`{"error":{"message":"usage limit exceeded","code":2.056e3}}`), codeadapter.CooldownQuota, codeadapter.ScopeCredential},
	}
	for _, tc := range cases {
		got, ok := vendor.Classify(endpoint, 429, tc.body, http.Header{}, now)
		if !ok || got.Class != tc.class || got.Scope != tc.scope || got.Envelope != tc.envelope || got.NotExecuted || got.SuppressFallback {
			t.Fatalf("Classify(%s) = %+v, %v", tc.name, got, ok)
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
	// Sources: https://platform.minimax.io/docs/api-reference/speech-to-text (fetched 2026-11-05) documents the exact OaiError envelope, and MiniMax-AI/cli@38aba5f (fetched 2026-11-05), src/errors/api.ts ApiErrorBody, declares status_code and error.code as numbers, so string or fractional codes and the parenthesized envelope with any member changed stay unclassified.
	vendor, endpoint := minimaxVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for name, body := range map[string][]byte{
		"string error code":      []byte(`{"error":{"code":"2056","message":"usage limit exceeded"}}`),
		"fractional error code":  []byte(`{"error":{"code":2056.5,"message":"usage limit exceeded"}}`),
		"string status code":     []byte(`{"base_resp":{"status_code":"2056","status_msg":"usage limit exceeded"}}`),
		"fractional status code": []byte(`{"base_resp":{"status_code":2056.5,"status_msg":"usage limit exceeded"}}`),
		"duplicate status code":  []byte(`{"base_resp":{"status_code":2056,"status_code":2056}}`),
		"escaped duplicate root": []byte(`{"base_resp":{"status_code":2056},"base_\u0072esp":{"status_code":2061},"error":{"code":2062}}`),
		"unknown declared code":  []byte(`{"base_resp":{"status_code":9999,"status_msg":"unknown"}}`),
		"stream event":           []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded (2056)"}}`),
		"missing request id":     []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded (2056)","http_code":"429"}}`),
		"extra root member":      []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded (2056)","http_code":"429"},"request_id":"r","extra":1}`),
		"http code mismatch":     []byte(`{"type":"error","error":{"type":"insufficient_balance_error","message":"usage limit exceeded (2056)","http_code":"402"},"request_id":"r"}`),
		"code not at end":        []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"(2056) usage limit exceeded","http_code":"429"},"request_id":"r"}`),
		"no space before code":   []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded(2056)","http_code":"429"},"request_id":"r"}`),
		"code only message":      []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"(2056)","http_code":"429"},"request_id":"r"}`),
		"unknown code":           minimaxBody("9999", "unknown"),
		"non digit suffix":       minimaxBody("205x", "usage limit exceeded"),
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
	// Sources: https://platform.minimax.io/docs/api-reference/errorcode (fetched 2026-11-05), codes 2056, 2067, https://platform.minimax.io/docs/api-reference/speech-to-text (fetched 2026-11-05), which documents the OaiError envelope only for speech-to-text, and MiniMax-AI/cli@38aba5f (fetched 2026-11-05), src/errors/api.ts, which declares the folded envelopes without proving non execution. Replay waits for live envelope qualification and no MiniMax shape replays today, proven or ambiguous
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
		"extra root member":  []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded (2056)","http_code":"429"},"request_id":"r","extra":1}`),
		"entitlement":        minimaxBody("2061", "your current token plan not support model"),
		"rate":               minimaxBody("1002", "rate limit, please retry later"),
		"native base_resp":   []byte(`{"base_resp":{"status_code":2056,"status_msg":"usage limit exceeded"}}`),
		"numeric error code": []byte(`{"error":{"message":"usage limit exceeded","type":"rate_limit_error","code":2056,"http_code":429}}`),
		"folded base_resp":   []byte(`{"base_resp":{"status_code":2061,"status_msg":"your current token plan not support model"},"error":{"message":"usage limit exceeded","type":"rate_limit_error","code":2056,"http_code":"429"}}`),
	} {
		if rejection, ok := vendor.InitialRejection(endpoint, 429, body, http.Header{}, now); ok {
			t.Fatalf("InitialRejection(%s) = %+v, want unproven", name, rejection)
		}
	}
	if rejection, ok := vendor.InitialRejection(endpoint, 401, minimaxBody("2056", "usage limit exceeded"), http.Header{}, now); ok {
		t.Fatalf("InitialRejection(401) = %+v, want unproven", rejection)
	}
}
