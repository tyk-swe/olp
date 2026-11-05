package codeadapter_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

func kimiVendor(t *testing.T) (codeadapter.Vendor, codeadapter.Endpoint, codeadapter.Endpoint) {
	t.Helper()
	vendor, ok := codeadapter.Lookup(codemode.AdapterKimiCode)
	if !ok {
		t.Fatal("kimi vendor missing")
	}
	chat, ok := vendor.Endpoint("v1/chat/completions")
	if !ok {
		t.Fatal("kimi chat endpoint missing")
	}
	messages, ok := vendor.Endpoint("v1/messages")
	if !ok {
		t.Fatal("kimi messages endpoint missing")
	}
	return vendor, chat, messages
}

func kimiChatBody(message string) []byte {
	return []byte(`{"error":{"message":"` + message + `","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
}

func kimiMessagesBody(message string) []byte {
	return []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"` + message + `"}}`)
}

const (
	kimiQuotaFiveHour      = "You've reached your 5-hour usage limit. Your quota will reset when the current 5-hour window ends. To continue now, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota"
	kimiQuotaWeekly        = "You've reached your weekly (7-day) usage limit. Your quota will reset when the current 7-day window ends. To continue now, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota"
	kimiQuotaMonthly       = "You've reached your monthly usage limit for this billing cycle. Your quota will be refreshed in the next cycle. To continue now, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota"
	kimiCreditInsufficient = "Your credit balance is insufficient. To continue, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota"
	kimiOrgExtraUsageOut   = "Your organization's Extra Usage balance has run out. Please contact your organization admin to add more Extra Usage."
	kimiOrgExtraUsageLimit = "You've reached the Extra Usage limit set for your account by your organization. Please contact your organization admin to increase your limit."
	kimiConcurrentLimit    = "You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again."
	kimiTooManyRequests    = "We're receiving too many requests at the moment. Please wait a moment and try again."
	kimiEngineOverloaded   = "The engine is currently overloaded, please try again later"
)

func TestClassifyKimiQuotaMessagesAreCredentialQuotaHeldPendingQualification(t *testing.T) {
	// Source: https://www.kimi.com/code/docs/en/kimi-code/error-reference.html (fetched 2026-11-05), exact HTTP 429 messages. That vendor doc proves the messages and HTTP 429 only. It does not prove the inference error envelope, so the fixture envelopes stay unproven until live qualification, quota keeps its cooldown class, and NotExecuted stays false
	vendor, chat, messages := kimiVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	cases := []struct{ message, envelope string }{
		{kimiQuotaFiveHour, "kimi_quota_five_hour"},
		{kimiQuotaWeekly, "kimi_quota_weekly"},
		{kimiQuotaMonthly, "kimi_quota_monthly"},
		{kimiCreditInsufficient, "kimi_credit_insufficient"},
		{kimiOrgExtraUsageOut, "kimi_org_extra_usage_exhausted"},
		{kimiOrgExtraUsageLimit, "kimi_org_extra_usage_limit"},
	}
	for _, tc := range cases {
		for name, attempt := range map[string]struct {
			endpoint codeadapter.Endpoint
			body     []byte
		}{
			"chat":     {chat, kimiChatBody(tc.message)},
			"messages": {messages, kimiMessagesBody(tc.message)},
		} {
			got, ok := vendor.Classify(attempt.endpoint, 429, attempt.body, http.Header{}, now)
			if !ok || got.Class != codeadapter.CooldownQuota || got.Scope != codeadapter.ScopeCredential || !got.RetryAt.Equal(now.Add(time.Minute)) || got.Envelope != tc.envelope || got.NotExecuted || got.SuppressFallback {
				t.Fatalf("Classify(%s, %s) = %+v, %v", tc.envelope, name, got, ok)
			}
		}
	}
}

func TestClassifyKimiRetryAtUsesOnlySingleValidRetryAfterElseMinute(t *testing.T) {
	// Source: https://www.kimi.com/code/docs/en/kimi-code/error-reference.html (fetched 2026-11-05); the messages carry no reset timestamp, so no deadline is ever derived from a message or subscription anchor.
	vendor, chat, _ := kimiVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		headers http.Header
		want    time.Time
	}{
		{"single retry-after", http.Header{"Retry-After": {"300"}}, now.Add(300 * time.Second)},
		{"multiple retry-after values ignored", http.Header{"Retry-After": {"300", "600"}}, now.Add(time.Minute)},
		{"invalid retry-after ignored", http.Header{"Retry-After": {"soon"}}, now.Add(time.Minute)},
		{"zero retry-after ignored", http.Header{"Retry-After": {"0"}}, now.Add(time.Minute)},
		{"no retry-after", http.Header{}, now.Add(time.Minute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, message := range []string{kimiQuotaMonthly, kimiConcurrentLimit, kimiTooManyRequests, kimiEngineOverloaded} {
				got, ok := vendor.Classify(chat, 429, kimiChatBody(message), tc.headers, now)
				if !ok || !got.RetryAt.Equal(tc.want) {
					t.Fatalf("Classify(%q) = %+v, %v want retry at %s", message, got, ok, tc.want)
				}
			}
		})
	}
}

func TestClassifyKimiShortOutcomesStayApartAndNeverReplay(t *testing.T) {
	// Source: https://www.kimi.com/code/docs/en/kimi-code/error-reference.html (fetched 2026-11-05), exact HTTP 429 messages.
	vendor, chat, messages := kimiVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		message, envelope string
		class             codeadapter.CooldownClass
		scope             codeadapter.CooldownScope
	}{
		{kimiConcurrentLimit, "kimi_concurrent_limit", codeadapter.CooldownConcurrency, codeadapter.ScopeCredential},
		{kimiTooManyRequests, "kimi_too_many_requests", codeadapter.CooldownRate, codeadapter.ScopeCredential},
		{kimiEngineOverloaded, "kimi_engine_overloaded", codeadapter.CooldownCapacity, codeadapter.ScopeModel},
	}
	for _, tc := range cases {
		for _, body := range [][]byte{kimiChatBody(tc.message), kimiMessagesBody(tc.message)} {
			got, ok := vendor.Classify(chat, 429, body, http.Header{}, now)
			if !ok || got.Class != tc.class || got.Scope != tc.scope || !got.RetryAt.Equal(now.Add(time.Minute)) || got.Envelope != tc.envelope || got.NotExecuted {
				t.Fatalf("Classify(%s) = %+v, %v", tc.envelope, got, ok)
			}
		}
		if rejection, ok := vendor.InitialRejection(messages, 429, kimiMessagesBody(tc.message), http.Header{}, now); ok {
			t.Fatalf("InitialRejection(%s) = %+v, want no replay proof", tc.envelope, rejection)
		}
	}
}

func TestClassifyKimiNearMissMessagesAndShapesStayUnclassified(t *testing.T) {
	// Source: https://www.kimi.com/code/docs/en/kimi-code/error-reference.html (fetched 2026-11-05); only the complete documented message matches.
	vendor, chat, _ := kimiVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for name, body := range map[string][]byte{
		"truncated quota message":          kimiChatBody("You've reached your 5-hour usage limit."),
		"quota message with trailing text": kimiChatBody(kimiQuotaFiveHour + " Please wait."),
		"overload prefix only":             kimiChatBody("The engine is currently overloaded"),
		"numeric message":                  []byte(`{"error":{"message":429}}`),
		"missing message":                  []byte(`{"error":{"type":"rate_limit_error"}}`),
		"extra root member":                []byte(`{"error":{"message":"` + kimiQuotaFiveHour + `"},"extra":1}`),
		"wrapped in another root member":   []byte(`{"result":{"error":{"message":"` + kimiQuotaFiveHour + `"}}}`),
		"anthropic shape with wrong type":  []byte(`{"type":"message","error":{"message":"` + kimiQuotaFiveHour + `"}}`),
	} {
		if got, ok := vendor.Classify(chat, 429, body, http.Header{}, now); ok {
			t.Fatalf("Classify(%s) = %+v, want unclassified", name, got)
		}
	}
	if got, ok := vendor.Classify(chat, 500, kimiChatBody(kimiQuotaFiveHour), http.Header{}, now); ok {
		t.Fatalf("Classify(500) = %+v, want unclassified", got)
	}
	other, found := codeadapter.Lookup(codemode.AdapterZAICoding)
	if !found {
		t.Fatal("glm vendor missing")
	}
	if got, ok := other.Classify(chat, 429, kimiChatBody(kimiQuotaFiveHour), http.Header{}, now); ok {
		t.Fatalf("glm Classify(kimi body) = %+v, want unclassified", got)
	}
}

func TestKimiInitialRejectionHeldUntilEnvelopeQualified(t *testing.T) {
	// Source: https://www.kimi.com/code/docs/en/kimi-code/error-reference.html (fetched 2026-11-05), exact HTTP 429 messages. That vendor doc proves the messages and HTTP 429 only, not the inference error envelope, so replay waits for live envelope qualification and no Kimi shape replays today, quota or ambiguous
	vendor, chat, messages := kimiVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for name, attempt := range map[string]struct {
		endpoint codeadapter.Endpoint
		body     []byte
	}{
		"chat":     {chat, kimiChatBody(kimiQuotaFiveHour)},
		"messages": {messages, kimiMessagesBody(kimiQuotaFiveHour)},
	} {
		if rejection, ok := vendor.InitialRejection(attempt.endpoint, 429, attempt.body, http.Header{"Retry-After": {"300"}}, now); ok {
			t.Fatalf("InitialRejection(%s) = %+v, want held pending live envelope qualification", name, rejection)
		}
	}
	for name, body := range map[string][]byte{
		"engine overloaded": kimiChatBody(kimiEngineOverloaded),
		"extra root member": []byte(`{"error":{"message":"` + kimiQuotaFiveHour + `"},"extra":1}`),
	} {
		if rejection, ok := vendor.InitialRejection(chat, 429, body, http.Header{}, now); ok {
			t.Fatalf("InitialRejection(%s) = %+v, want unproven", name, rejection)
		}
	}
	if rejection, ok := vendor.InitialRejection(chat, 401, kimiChatBody(kimiQuotaFiveHour), http.Header{}, now); ok {
		t.Fatalf("InitialRejection(401) = %+v, want unproven", rejection)
	}
}
