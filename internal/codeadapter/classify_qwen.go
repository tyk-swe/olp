package codeadapter

import (
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/upstream"
)

const (
	qwenQuotaEnvelope = "qwen_insufficient_quota"
	qwenRateEnvelope  = "qwen_requests_rate_limit_exceeded"
	qwenQuotaMessage  = "Your token-plan quota has been exhausted."
	qwenRatePrefix    = "Requests rate limit exceeded"
)

func classifyQwen(status int, body []byte, headers http.Header, now time.Time) (Classification, bool) {
	if status != 429 {
		return Classification{}, false
	}
	code, message, ok := qwenEnvelope(body)
	if !ok {
		return Classification{}, false
	}
	if code == "insufficient_quota" && message == qwenQuotaMessage {
		return Classification{Class: CooldownQuota, Scope: ScopeCredential, RetryAt: qwenRetryAt(headers, now), Envelope: qwenQuotaEnvelope, NotExecuted: true}, true
	}
	if strings.HasPrefix(message, qwenRatePrefix) {
		return Classification{Class: CooldownRate, Scope: ScopeCredential, RetryAt: qwenRetryAt(headers, now), Envelope: qwenRateEnvelope}, true
	}
	return Classification{}, false
}

func qwenEnvelope(body []byte) (string, string, bool) {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 64 << 10})
	if err != nil || document.Root().Kind() != oif.Object {
		return "", "", false
	}
	code, codeOK := qwenText(document, "/error/code")
	message, messageOK := qwenText(document, "/error/message")
	if !messageOK {
		code, codeOK = qwenText(document, "/code")
		message, messageOK = qwenText(document, "/message")
	}
	if !messageOK || message == "" {
		return "", "", false
	}
	if !codeOK {
		code = ""
	}
	return code, message, true
}

func qwenText(document oif.Document, pointer string) (string, bool) {
	value, found := document.Lookup(pointer)
	if !found {
		return "", false
	}
	return value.Text()
}

func qwenRetryAt(headers http.Header, now time.Time) time.Time {
	retryAt := now.Add(time.Minute)
	if values := headers.Values("Retry-After"); len(values) == 1 {
		if delay, valid := upstream.ParseRetryAfter(values[0], now); valid && now.Add(delay).After(retryAt) {
			return now.Add(delay)
		}
	}
	return retryAt
}
