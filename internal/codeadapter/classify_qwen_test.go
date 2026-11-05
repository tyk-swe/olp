package codeadapter_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/upstream"
)

func qwenVendor(t *testing.T) (codeadapter.Vendor, codeadapter.Endpoint) {
	t.Helper()
	vendor, ok := codeadapter.Lookup(codemode.AdapterQwenTokenPlan)
	if !ok {
		t.Fatal("qwen vendor missing")
	}
	endpoint, ok := vendor.Endpoint("v1/chat/completions")
	if !ok {
		t.Fatal("qwen chat endpoint missing")
	}
	return vendor, endpoint
}

func qwenQuotaBody() []byte {
	return []byte(`{"error":{"message":"Your token-plan quota has been exhausted.","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`)
}

func TestClassifyQwenInsufficientQuotaIsCredentialQuota(t *testing.T) {
	// Source: https://www.alibabacloud.com/help/en/model-studio/token-plan-team-faq (fetched 2026-11-05), 429 insufficient_quota, seat monthly quota exhausted.
	vendor, endpoint := qwenVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	got, ok := vendor.Classify(endpoint, 429, qwenQuotaBody(), http.Header{}, now)
	if !ok || got.Class != codeadapter.CooldownQuota || got.Scope != codeadapter.ScopeCredential || got.Envelope != "qwen_insufficient_quota" || !got.NotExecuted || !got.RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("Classify = %+v, %v", got, ok)
	}
	got, ok = vendor.Classify(endpoint, 429, qwenQuotaBody(), http.Header{"Retry-After": {"3600"}}, now)
	if !ok || !got.RetryAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("Classify with retry-after = %+v, %v", got, ok)
	}
	got, ok = vendor.Classify(endpoint, 429, []byte(`{"type":"error","error":{"type":"rate_limit_error","code":"insufficient_quota","message":"Your token-plan quota has been exhausted."}}`), http.Header{}, now)
	if !ok || got.Class != codeadapter.CooldownQuota || got.Envelope != "qwen_insufficient_quota" || !got.NotExecuted {
		t.Fatalf("Classify messages shape = %+v, %v", got, ok)
	}
	got, ok = vendor.Classify(endpoint, 429, []byte(`{"code":"insufficient_quota","message":"Your token-plan quota has been exhausted.","request_id":"649b2bbc-c541-9e16-9845-db7fe4fe5b2d"}`), http.Header{}, now)
	if !ok || got.Class != codeadapter.CooldownQuota || got.Envelope != "qwen_insufficient_quota" {
		t.Fatalf("Classify native shape = %+v, %v", got, ok)
	}
}

func TestClassifyQwenRequestsRateLimitIsShortRate(t *testing.T) {
	// Source: https://www.alibabacloud.com/help/en/model-studio/token-plan-team-faq (fetched 2026-11-05), 429 Requests rate limit exceeded, wait one minute.
	// Source: https://www.alibabacloud.com/help/en/model-studio/error-code (fetched 2026-11-05), Code Throttling.RateQuota, Message "Requests rate limit exceeded, please try again later."
	vendor, endpoint := qwenVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for _, body := range [][]byte{
		[]byte(`{"error":{"message":"Requests rate limit exceeded, please try again later.","type":"rate_limit_error","code":"Throttling.RateQuota"}}`),
		[]byte(`{"code":"Throttling.RateQuota","message":"Requests rate limit exceeded, please try again later.","request_id":"649b2bbc-c541-9e16-9845-db7fe4fe5b2d"}`),
	} {
		got, ok := vendor.Classify(endpoint, 429, body, http.Header{}, now)
		if !ok || got.Class != codeadapter.CooldownRate || got.Scope != codeadapter.ScopeCredential || got.Envelope != "qwen_requests_rate_limit_exceeded" || got.NotExecuted || !got.RetryAt.Equal(now.Add(time.Minute)) {
			t.Fatalf("Classify(%s) = %+v, %v", body, got, ok)
		}
	}
}

func TestClassifyQwenCodingPlanAllocatedQuotaNeverClassifies(t *testing.T) {
	// Source: https://www.alibabacloud.com/help/en/model-studio/error-code (fetched 2026-11-05), Throttling.AllocationQuota/insufficient_quota TPS/TPM messages.
	// Source: https://www.alibabacloud.com/help/en/model-studio/coding-plan-faq via docs/research/cliproxyapi-quota-signals.md (fetched 2026-11-05), separate Coding Plan 5 hour, weekly and monthly allocated quota messages.
	vendor, endpoint := qwenVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for name, body := range map[string][]byte{
		"allocation quota with quota code":      []byte(`{"error":{"code":"insufficient_quota","message":"Allocated quota exceeded, please increase your quota limit."}}`),
		"generic quota message with quota code": []byte(`{"error":{"code":"insufficient_quota","message":"You exceeded your current quota, please check your plan and billing details."}}`),
		"quota message without code":            []byte(`{"error":{"message":"Your token-plan quota has been exhausted."}}`),
		"quota message with other code":         []byte(`{"error":{"code":"Throttling.AllocationQuota","message":"Your token-plan quota has been exhausted."}}`),
		"five hour allocated quota":             []byte(`{"error":{"code":"Throttling.AllocationQuota","message":"Your allocated quota for the past 5 hours has been exhausted. It will reset in the next 5-hour window."}}`),
		"weekly allocated quota":                []byte(`{"error":{"code":"Throttling.AllocationQuota","message":"Your weekly allocated quota has been exhausted. It will reset next week."}}`),
		"monthly allocated quota":               []byte(`{"error":{"code":"Throttling.AllocationQuota","message":"Your monthly allocated quota has been exhausted. It will reset next month."}}`),
		"free tier only":                        []byte(`{"error":{"code":"AllocationQuota.FreeTierOnly","message":"The free tier of the model has been exhausted."}}`),
		"rate message without code":             []byte(`{"error":{"message":"Requests rate limit exceeded, please try again later."}}`),
		"rate message with quota code":          []byte(`{"error":{"code":"insufficient_quota","message":"Requests rate limit exceeded, please try again later."}}`),
		"rate message with allocation code":     []byte(`{"error":{"code":"Throttling.AllocationQuota","message":"Requests rate limit exceeded, please try again later."}}`),
		"rate message native without code":      []byte(`{"message":"Requests rate limit exceeded, please try again later.","request_id":"649b2bbc-c541-9e16-9845-db7fe4fe5b2d"}`),
		"quota code on wrong status":            []byte(`{"error":{"code":"insufficient_quota","message":"Your token-plan quota has been exhausted."}}`),
	} {
		status := 429
		if name == "quota code on wrong status" {
			status = 403
		}
		if got, ok := vendor.Classify(endpoint, status, body, http.Header{}, now); ok {
			t.Fatalf("Classify(%s) = %+v, want unclassified", name, got)
		}
	}
	other, found := codeadapter.Lookup(codemode.AdapterZAICoding)
	if !found {
		t.Fatal("glm vendor missing")
	}
	if got, ok := other.Classify(endpoint, 429, qwenQuotaBody(), http.Header{}, now); ok {
		t.Fatalf("glm Classify(qwen body) = %+v, want unclassified", got)
	}
}

func TestQwenInitialRejectionOnlyForProvenQuotaEnvelope(t *testing.T) {
	// Source: https://www.alibabacloud.com/help/en/model-studio/token-plan-team-faq (fetched 2026-11-05), 429 insufficient_quota; complete unary body before any content is what gateway recover() hands over.
	vendor, endpoint := qwenVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	rejection, ok := vendor.InitialRejection(endpoint, 429, qwenQuotaBody(), http.Header{}, now)
	if !ok || rejection.Class != upstream.RateLimit || rejection.Envelope != "qwen_insufficient_quota" || !rejection.RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("InitialRejection(proven) = %+v, %v", rejection, ok)
	}
	for name, body := range map[string][]byte{
		"rate":              []byte(`{"error":{"code":"Throttling.RateQuota","message":"Requests rate limit exceeded, please try again later."}}`),
		"coding plan quota": []byte(`{"error":{"code":"insufficient_quota","message":"Allocated quota exceeded, please increase your quota limit."}}`),
	} {
		if rejection, ok := vendor.InitialRejection(endpoint, 429, body, http.Header{}, now); ok {
			t.Fatalf("InitialRejection(%s) = %+v, want unproven", name, rejection)
		}
	}
	if rejection, ok := vendor.InitialRejection(endpoint, 401, qwenQuotaBody(), http.Header{}, now); ok {
		t.Fatalf("InitialRejection(401) = %+v, want unproven", rejection)
	}
}
