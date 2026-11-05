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

func TestClassifyKimiQuotaMessagesAreCredentialQuotaWithoutReplay(t *testing.T) {
	// Sources: https://www.kimi.com/code/docs/en/kimi-code/error-reference.html (fetched 2026-11-05), exact HTTP 429 messages, and MoonshotAI/kimi-code@df90d90 (fetched 2026-11-05), whose classifyKimiQuotaError keys on the structured code only and refuses message wording, so a message match keeps its cooldown class but NotExecuted stays false
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
		{"zero retry-after", http.Header{"Retry-After": {"0"}}, now},
		{"past retry-after", http.Header{"Retry-After": {now.Add(-time.Minute).Format(http.TimeFormat)}}, now},
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
	// Sources: https://www.kimi.com/code/docs/en/kimi-code/error-reference.html (fetched 2026-11-05), only the complete documented message matches a quota outcome, and MoonshotAI/kimi-code@df90d90 (fetched 2026-11-05), whose rate codes classify a near miss carrying them as plain rate and never as quota
	vendor, chat, _ := kimiVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for name, body := range map[string][]byte{
		"truncated quota message":          kimiChatBody("You've reached your 5-hour usage limit."),
		"quota message with trailing text": kimiChatBody(kimiQuotaFiveHour + " Please wait."),
		"overload prefix only":             kimiChatBody("The engine is currently overloaded"),
	} {
		got, ok := vendor.Classify(chat, 429, body, http.Header{}, now)
		if !ok || got.Class != codeadapter.CooldownRate || got.Scope != codeadapter.ScopeCredential || got.Envelope != "kimi_rate_limit_exceeded" || got.NotExecuted {
			t.Fatalf("Classify(%s) = %+v, %v, want structured rate and never quota", name, got, ok)
		}
	}
	for name, body := range map[string][]byte{
		"numeric message":                 []byte(`{"error":{"message":429}}`),
		"missing message":                 []byte(`{"error":{"type":"rate_limit_error"}}`),
		"extra root member":               []byte(`{"error":{"message":"` + kimiQuotaFiveHour + `"},"extra":1}`),
		"wrapped in another root member":  []byte(`{"result":{"error":{"message":"` + kimiQuotaFiveHour + `"}}}`),
		"anthropic shape with wrong type": []byte(`{"type":"message","error":{"message":"` + kimiQuotaFiveHour + `"}}`),
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

func TestClassifyKimiStructuredQuotaCodesKeepCooldownWithoutReplay(t *testing.T) {
	// Sources: MoonshotAI/kimi-code@df90d90 (fetched 2026-11-05), classifyKimiQuotaError keys on code or type equal to exceeded_current_quota_error at HTTP 429 while the base OpenAI decoder also treats insufficient_quota and the Anthropic decoder reads the nested error type, and https://www.kimi.com/code/docs/en/kimi-code/error-reference.html (fetched 2026-11-05) for the 429 quota context. Member sets are pinned to exactly what those decoders read, so message wording is required to exist but never matched on this path
	vendor, chat, messages := kimiVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for name, attempt := range map[string]struct {
		endpoint codeadapter.Endpoint
		body     []byte
		envelope string
	}{
		"chat code exceeded":          {chat, []byte(`{"error":{"message":"quota exceeded","type":"rate_limit_error","code":"exceeded_current_quota_error"}}`), "kimi_exceeded_current_quota_error"},
		"chat type exceeded":          {chat, []byte(`{"error":{"message":"quota exceeded","type":"exceeded_current_quota_error"}}`), "kimi_exceeded_current_quota_error"},
		"chat code insufficient":      {chat, []byte(`{"error":{"message":"quota exceeded","type":"insufficient_quota","code":"insufficient_quota"}}`), "kimi_insufficient_quota"},
		"chat code only insufficient": {chat, []byte(`{"error":{"message":"quota exceeded","code":"insufficient_quota"}}`), "kimi_insufficient_quota"},
		"messages type exceeded":      {messages, []byte(`{"type":"error","error":{"type":"exceeded_current_quota_error","message":"quota exceeded"}}`), "kimi_exceeded_current_quota_error"},
		"messages type insufficient":  {messages, []byte(`{"type":"error","error":{"type":"insufficient_quota","message":"quota exceeded"}}`), "kimi_insufficient_quota"},
	} {
		got, ok := vendor.Classify(attempt.endpoint, 429, attempt.body, http.Header{}, now)
		if !ok || got.Class != codeadapter.CooldownQuota || got.Scope != codeadapter.ScopeCredential || !got.RetryAt.Equal(now.Add(time.Minute)) || got.Envelope != attempt.envelope || got.NotExecuted || got.SuppressFallback {
			t.Fatalf("Classify(%s) = %+v, %v", name, got, ok)
		}
		if rejection, ok := vendor.InitialRejection(attempt.endpoint, 429, attempt.body, http.Header{}, now); ok {
			t.Fatalf("InitialRejection(%s) = %+v, want no replay", name, rejection)
		}
	}
}

func TestClassifyKimiStructuredQuotaOnDifferentEndpointKeepsCooldownWithoutReplay(t *testing.T) {
	vendor, chat, messages := kimiVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for name, attempt := range map[string]struct {
		endpoint codeadapter.Endpoint
		body     []byte
		envelope string
	}{
		"chat envelope on messages endpoint": {messages, []byte(`{"error":{"message":"quota exceeded","type":"rate_limit_error","code":"exceeded_current_quota_error"}}`), "kimi_exceeded_current_quota_error"},
		"messages envelope on chat endpoint": {chat, []byte(`{"type":"error","error":{"type":"insufficient_quota","message":"quota exceeded"}}`), "kimi_insufficient_quota"},
	} {
		got, ok := vendor.Classify(attempt.endpoint, 429, attempt.body, http.Header{}, now)
		if !ok || got.Class != codeadapter.CooldownQuota || got.Envelope != attempt.envelope || got.NotExecuted {
			t.Fatalf("Classify(%s) = %+v, %v, want quota cooldown without proof", name, got, ok)
		}
		if rejection, ok := vendor.InitialRejection(attempt.endpoint, 429, attempt.body, http.Header{}, now); ok {
			t.Fatalf("InitialRejection(%s) = %+v, want no replay across endpoints", name, rejection)
		}
	}
}

func TestKimiInitialRejectionHoldsUnprovenEnvelopes(t *testing.T) {
	vendor, chat, messages := kimiVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for name, attempt := range map[string]struct {
		endpoint codeadapter.Endpoint
		body     []byte
	}{
		"message-only chat quota":     {chat, kimiChatBody(kimiQuotaFiveHour)},
		"message-only messages quota": {messages, kimiMessagesBody(kimiQuotaFiveHour)},
	} {
		got, ok := vendor.Classify(attempt.endpoint, 429, attempt.body, http.Header{}, now)
		if !ok || got.Class != codeadapter.CooldownQuota || got.NotExecuted {
			t.Fatalf("Classify(%s) = %+v, %v, want quota cooldown without proof", name, got, ok)
		}
		if rejection, ok := vendor.InitialRejection(attempt.endpoint, 429, attempt.body, http.Header{"Retry-After": {"300"}}, now); ok {
			t.Fatalf("InitialRejection(%s) = %+v, want no replay from message wording", name, rejection)
		}
	}
	for name, body := range map[string][]byte{
		"chat extra param member":          []byte(`{"error":{"message":"quota exceeded","type":"exceeded_current_quota_error","code":"exceeded_current_quota_error","param":null}}`),
		"chat string extra error member":   []byte(`{"error":{"message":"quota exceeded","code":"exceeded_current_quota_error","param":"model"}}`),
		"chat extra root member":           []byte(`{"error":{"message":"quota exceeded","code":"exceeded_current_quota_error"},"extra":1}`),
		"chat duplicate root error member": []byte(`{"error":{"message":"quota exceeded","code":"exceeded_current_quota_error"},"error":{"message":"quota exceeded","code":"exceeded_current_quota_error"}}`),
		"chat duplicate inner code member": []byte(`{"error":{"message":"quota exceeded","code":"exceeded_current_quota_error","code":"exceeded_current_quota_error"}}`),
		"chat duplicate inner message":     []byte(`{"error":{"message":"quota exceeded","message":"quota exceeded","code":"exceeded_current_quota_error"}}`),
		"messages duplicate root type":     []byte(`{"type":"error","type":"error","error":{"type":"exceeded_current_quota_error","message":"quota exceeded"}}`),
		"messages duplicate inner type":    []byte(`{"type":"error","error":{"type":"exceeded_current_quota_error","type":"exceeded_current_quota_error","message":"quota exceeded"}}`),
		"chat numeric code":                []byte(`{"error":{"message":"quota exceeded","code":429}}`),
		"chat missing message":             []byte(`{"error":{"code":"exceeded_current_quota_error","type":"exceeded_current_quota_error"}}`),
		"messages extra error code":        []byte(`{"type":"error","error":{"type":"exceeded_current_quota_error","message":"quota exceeded","code":"exceeded_current_quota_error"}}`),
		"messages extra root member":       []byte(`{"type":"error","error":{"type":"exceeded_current_quota_error","message":"quota exceeded"},"request_id":"r"}`),
		"messages code not type":           []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"quota exceeded"}}`),
		"engine overloaded message":        kimiChatBody(kimiEngineOverloaded),
	} {
		if got, ok := vendor.Classify(chat, 429, body, http.Header{}, now); ok && got.NotExecuted {
			t.Fatalf("Classify(%s) = %+v, want no NotExecuted proof", name, got)
		}
		if rejection, ok := vendor.InitialRejection(chat, 429, body, http.Header{}, now); ok {
			t.Fatalf("InitialRejection(%s) = %+v, want unproven", name, rejection)
		}
	}
	if rejection, ok := vendor.InitialRejection(messages, 401, []byte(`{"type":"error","error":{"type":"exceeded_current_quota_error","message":"quota exceeded"}}`), http.Header{}, now); ok {
		t.Fatalf("InitialRejection(401) = %+v, want unproven", rejection)
	}
}

func TestClassifyKimiStructuredOverloadAndRateCodesAreCapacityAndRateNeverQuota(t *testing.T) {
	// Source: MoonshotAI/kimi-code@df90d90 (fetched 2026-11-05), the base decoders check structured codes ahead of status and wording so overloaded_error and engine_overloaded_error land overloaded and rate_limit_exceeded, rate_limit_reached_error and rate_limit_error land rate_limit, and only exceeded_current_quota_error and insufficient_quota are quota codes
	vendor, chat, messages := kimiVendor(t)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for name, attempt := range map[string]struct {
		endpoint codeadapter.Endpoint
		body     []byte
		class    codeadapter.CooldownClass
		scope    codeadapter.CooldownScope
		envelope string
	}{
		"chat overloaded_error":                     {chat, []byte(`{"error":{"message":"overloaded","type":"overloaded_error","code":"overloaded_error"}}`), codeadapter.CooldownCapacity, codeadapter.ScopeModel, "kimi_overloaded_error"},
		"chat engine_overloaded_error code":         {chat, []byte(`{"error":{"message":"overloaded","code":"engine_overloaded_error"}}`), codeadapter.CooldownCapacity, codeadapter.ScopeModel, "kimi_engine_overloaded_error"},
		"chat overloaded type with rate code":       {chat, []byte(`{"error":{"message":"overloaded","type":"engine_overloaded_error","code":"rate_limit_exceeded"}}`), codeadapter.CooldownCapacity, codeadapter.ScopeModel, "kimi_engine_overloaded_error"},
		"chat quota message with overload code":     {chat, []byte(`{"error":{"message":"` + kimiQuotaFiveHour + `","type":"rate_limit_error","code":"engine_overloaded_error"}}`), codeadapter.CooldownCapacity, codeadapter.ScopeModel, "kimi_engine_overloaded_error"},
		"chat rate_limit_exceeded code":             {chat, []byte(`{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit_exceeded"}}`), codeadapter.CooldownRate, codeadapter.ScopeCredential, "kimi_rate_limit_exceeded"},
		"chat rate_limit_reached_error type":        {chat, []byte(`{"error":{"message":"slow down","type":"rate_limit_reached_error"}}`), codeadapter.CooldownRate, codeadapter.ScopeCredential, "kimi_rate_limit_reached_error"},
		"messages overloaded_error":                 {messages, []byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`), codeadapter.CooldownCapacity, codeadapter.ScopeModel, "kimi_overloaded_error"},
		"messages quota message with overload type": {messages, []byte(`{"type":"error","error":{"type":"overloaded_error","message":"` + kimiQuotaFiveHour + `"}}`), codeadapter.CooldownCapacity, codeadapter.ScopeModel, "kimi_overloaded_error"},
		"messages rate_limit_error":                 {messages, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`), codeadapter.CooldownRate, codeadapter.ScopeCredential, "kimi_rate_limit_error"},
	} {
		got, ok := vendor.Classify(attempt.endpoint, 429, attempt.body, http.Header{}, now)
		if !ok || got.Class != attempt.class || got.Class == codeadapter.CooldownQuota || got.Scope != attempt.scope || got.Envelope != attempt.envelope || !got.RetryAt.Equal(now.Add(time.Minute)) || got.NotExecuted {
			t.Fatalf("Classify(%s) = %+v, %v", name, got, ok)
		}
		if rejection, ok := vendor.InitialRejection(attempt.endpoint, 429, attempt.body, http.Header{}, now); ok {
			t.Fatalf("InitialRejection(%s) = %+v, want unproven", name, rejection)
		}
	}
}
