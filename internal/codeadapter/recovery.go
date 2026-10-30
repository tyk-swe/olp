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

type Rejection struct {
	Class   upstream.Class
	RetryAt time.Time
}

func (v Vendor) InitialRejection(endpoint Endpoint, status int, body []byte, headers http.Header, now time.Time) (Rejection, bool) {
	if v.Adapter != codemode.AdapterCodex || endpoint.Observation != ObserveCodex || status != 401 && status != 429 {
		return Rejection{}, false
	}
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 64 << 10})
	if err != nil || document.Root().Kind() != oif.Object || len(document.Root().Members()) != 1 {
		return Rejection{}, false
	}
	errorType, _ := document.Lookup("/error/type")
	errorCode, _ := document.Lookup("/error/code")
	typeName, _ := errorType.Text()
	code, _ := errorCode.Text()
	if !(status == 401 && typeName == "authentication_error" && code == "invalid_api_key" || status == 429 && typeName == "usage_limit_reached") {
		return Rejection{}, false
	}
	outcome := (upstream.Classifier{AtMostOnce: true}).Classify(upstream.Evidence{Reached: true, Status: status, Error: &wire.UpstreamError{Type: typeName, Code: code}})
	if outcome.Acceptance != upstream.Terminal || outcome.Class != upstream.Credential && outcome.Class != upstream.RateLimit {
		return Rejection{}, false
	}
	reset, _ := document.Lookup("/error/resets_at")
	resetSeconds, _ := strconv.ParseInt(reset.Raw(), 10, 64)
	delay, _ := document.Lookup("/error/resets_in_seconds")
	delaySeconds, _ := strconv.ParseInt(delay.Raw(), 10, 32)
	retryAt := slices.MaxFunc([]time.Time{now.Add(time.Minute), retryAfter(headers, now), time.Unix(resetSeconds, 0), now.Add(time.Duration(delaySeconds) * time.Second)}, time.Time.Compare)
	return Rejection{Class: outcome.Class, RetryAt: retryAt}, true
}

func retryAfter(headers http.Header, now time.Time) time.Time {
	if seconds, err := strconv.ParseInt(headers.Get("Retry-After"), 10, 32); err == nil && seconds > 0 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	date, _ := http.ParseTime(headers.Get("Retry-After"))
	return date
}
