package codeadapter

import (
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

const QwenQuotaEnvelope = "qwen_insufficient_quota"

const (
	qwenRateEnvelope = "qwen_requests_rate_limit_exceeded"
	qwenQuotaMessage = "Your token-plan quota has been exhausted."
	qwenRateCode     = "Throttling.RateQuota"
	qwenRatePrefix   = "Requests rate limit exceeded"
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
		return Classification{Class: CooldownQuota, Scope: ScopeCredential, RetryAt: retryAtFrom(time.Time{}, false, headers, now), Envelope: QwenQuotaEnvelope, NotExecuted: true}, true
	}
	if code == qwenRateCode && strings.HasPrefix(message, qwenRatePrefix) {
		return Classification{Class: CooldownRate, Scope: ScopeCredential, RetryAt: retryAtFrom(time.Time{}, false, headers, now), Envelope: qwenRateEnvelope}, true
	}
	return Classification{}, false
}

func WithCycleReset(classification Classification, cycleAnchor *string, now time.Time) Classification {
	adjusted := classification
	adjusted.RetryAt = cycleResetAt(classification.Envelope, classification.RetryAt, cycleAnchor, now)
	return adjusted
}

func RejectionWithCycleReset(rejection Rejection, cycleAnchor *string, now time.Time) Rejection {
	adjusted := rejection
	adjusted.RetryAt = cycleResetAt(rejection.Envelope, rejection.RetryAt, cycleAnchor, now)
	return adjusted
}

func cycleResetAt(envelope string, retryAt time.Time, cycleAnchor *string, now time.Time) time.Time {
	if envelope != QwenQuotaEnvelope || cycleAnchor == nil {
		return retryAt
	}
	reset, ok := codemode.NextMonthlyReset(*cycleAnchor, now)
	if !ok || !reset.After(retryAt) {
		return retryAt
	}
	return reset
}

func qwenEnvelope(body []byte) (string, string, bool) {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 64 << 10})
	if err != nil || document.Root().Kind() != oif.Object {
		return "", "", false
	}
	code, message, messageOK := qwenCodeMessage(document)
	if !messageOK || message == "" {
		return "", "", false
	}
	return code, message, true
}

func qwenCodeMessage(document oif.Document) (string, string, bool) {
	message, messageOK := qwenText(document, "/error/message")
	if messageOK {
		code, _ := qwenText(document, "/error/code")
		return code, message, true
	}
	code, _ := qwenText(document, "/code")
	topMessage, topOK := qwenText(document, "/message")
	return code, topMessage, topOK
}

func qwenText(document oif.Document, pointer string) (string, bool) {
	value, found := document.Lookup(pointer)
	if !found {
		return "", false
	}
	return value.Text()
}
