package codeadapter

import (
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/upstream"
)

type CooldownClass = codemode.CooldownClass

const (
	CooldownQuota       = codemode.CooldownQuota
	CooldownEntitlement = codemode.CooldownEntitlement
	CooldownRate        = codemode.CooldownRate
	CooldownCapacity    = codemode.CooldownCapacity
	CooldownConcurrency = codemode.CooldownConcurrency
)

func CooldownClasses() [5]CooldownClass {
	return codemode.CooldownClasses()
}

type CooldownScope string

const (
	ScopeCredential CooldownScope = "credential"
	ScopeModel      CooldownScope = "model"
)

type Classification struct {
	Class            codemode.CooldownClass
	Scope            CooldownScope
	RetryAt          time.Time
	Envelope         string
	NotExecuted      bool
	SuppressFallback bool
}

func (v Vendor) Classify(endpoint Endpoint, status int, body []byte, headers http.Header, now time.Time) (Classification, bool) {
	switch v.Adapter {
	case codemode.AdapterCodex:
		return classifyCodex(endpoint, status, body, headers, now)
	case codemode.AdapterZAICoding:
		return classifyGLM(status, body, headers, now)
	case codemode.AdapterKimiCode:
		return classifyKimi(status, body, headers, now)
	case codemode.AdapterQwenTokenPlan:
		return classifyQwen(status, body, headers, now)
	case codemode.AdapterMiniMaxPlan:
		return classifyMiniMax(status, body, headers, now)
	}
	return Classification{}, false
}

func classifyCodex(endpoint Endpoint, status int, body []byte, headers http.Header, now time.Time) (Classification, bool) {
	if endpoint.Observation != ObserveCodex || status != 429 {
		return Classification{}, false
	}
	document, typeName, code, single, ok := rejectionDocument(body)
	if !ok {
		return Classification{}, false
	}
	if typeName == "usage_limit_reached" {
		notExecuted := false
		if single {
			outcome := (upstream.Classifier{AtMostOnce: true}).Classify(upstream.Evidence{Reached: true, Status: status, Error: &wire.UpstreamError{Type: typeName, Code: code}})
			notExecuted = outcome.Acceptance == upstream.Terminal && outcome.Class == upstream.RateLimit
		}
		return Classification{Class: CooldownQuota, Scope: ScopeCredential, RetryAt: rejectionRetryAt(document, headers, now), Envelope: "codex_usage_limit_reached", NotExecuted: notExecuted}, true
	}
	if slices.Contains([]string{"turn_rate_limit_exceeded", "model_at_capacity", "model_is_at_capacity"}, typeName) || slices.Contains([]string{"turn_rate_limit_exceeded", "model_at_capacity", "model_is_at_capacity"}, code) {
		envelope := "codex_model_at_capacity"
		if typeName == "turn_rate_limit_exceeded" || code == "turn_rate_limit_exceeded" {
			envelope = "codex_turn_rate_limit_exceeded"
		}
		return Classification{Class: CooldownCapacity, Scope: ScopeModel, RetryAt: rejectionRetryAt(document, headers, now), Envelope: envelope}, true
	}
	return Classification{}, false
}

func retryAtFrom(reset time.Time, hasReset bool, headers http.Header, now time.Time) time.Time {
	if hasReset && reset.After(now) {
		return reset
	}
	if values := headers.Values("Retry-After"); len(values) == 1 {
		if delay, valid := upstream.ParseRetryAfter(values[0], now); valid && delay > 0 {
			return now.Add(delay)
		}
	}
	return now.Add(time.Minute)
}

func rejectionDocument(body []byte) (oif.Document, string, string, bool, bool) {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 64 << 10})
	if err != nil || document.Root().Kind() != oif.Object {
		return oif.Document{}, "", "", false, false
	}
	errorType, _ := document.Lookup("/error/type")
	errorCode, _ := document.Lookup("/error/code")
	typeName, _ := errorType.Text()
	code, _ := errorCode.Text()
	if code == "" {
		topCode, _ := document.Lookup("/code")
		code, _ = topCode.Text()
	}
	return document, typeName, code, len(document.Root().Members()) == 1, true
}

func rejectionRetryAt(document oif.Document, headers http.Header, now time.Time) time.Time {
	candidates := []time.Time{}
	if values := headers.Values("Retry-After"); len(values) == 1 {
		if delay, valid := upstream.ParseRetryAfter(values[0], now); valid {
			candidates = append(candidates, now.Add(delay))
		}
	}
	reset, _ := document.Lookup("/error/resets_at")
	if seconds, err := strconv.ParseInt(reset.Raw(), 10, 64); err == nil && time.Unix(seconds, 0).After(now) {
		candidates = append(candidates, time.Unix(seconds, 0).UTC())
	}
	delay, _ := document.Lookup("/error/resets_in_seconds")
	if seconds, err := strconv.ParseInt(delay.Raw(), 10, 32); err == nil && seconds >= 0 {
		candidates = append(candidates, now.Add(time.Duration(seconds)*time.Second))
	}
	if len(candidates) == 0 {
		return now.Add(time.Minute)
	}
	return slices.MaxFunc(append(candidates, now), time.Time.Compare)
}
