package upstream

import (
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestClassifyInBandErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		classifier Classifier
		stated     wire.UpstreamError
		committed  bool
		want       Class
	}{
		{"context rejection", Classifier{ContextWindow: true}, wire.UpstreamError{Code: "context_window_exceeded"}, false, ContextWindow},
		{"context rejection without a window", Classifier{}, wire.UpstreamError{Code: "context_window_exceeded"}, false, ClientError},
		{"authentication type", Classifier{}, wire.UpstreamError{Type: "authentication_error"}, false, Credential},
		{"permission status code", Classifier{}, wire.UpstreamError{Code: "403"}, false, Credential},
		{"rate limit type", Classifier{}, wire.UpstreamError{Type: "rate_limit_error"}, false, RateLimit},
		{"exhausted resource", Classifier{}, wire.UpstreamError{Code: "RESOURCE_EXHAUSTED"}, false, RateLimit},
		{"throttling", Classifier{}, wire.UpstreamError{Type: "ThrottlingException"}, false, RateLimit},
		{"deadline", Classifier{}, wire.UpstreamError{Code: "DEADLINE_EXCEEDED"}, false, Timeout},
		{"invalid request", Classifier{}, wire.UpstreamError{Type: "invalid_request_error"}, true, ClientError},
		{"validation", Classifier{}, wire.UpstreamError{Type: "ValidationException"}, false, ClientError},
		{"unknown failure before commit", Classifier{}, wire.UpstreamError{Type: "overloaded_error"}, false, ServerError},
		{"unknown failure after commit", Classifier{}, wire.UpstreamError{Type: "overloaded_error"}, true, Protocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stated := tc.stated
			got := tc.classifier.Classify(Evidence{Reached: true, Accepted: true, Error: &stated, Committed: tc.committed})
			if got.Class != tc.want || got.Acceptance != Accepted || got.CredentialRefused != (tc.want == Credential) {
				t.Fatalf("outcome %+v, want class %q accepted", got, tc.want)
			}
		})
	}
}

func TestParseRetryAfterSignals(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		value     string
		delay     time.Duration
		supported bool
	}{
		{"seconds", "120", 2 * time.Minute, true},
		{"surrounding whitespace", " \t120 \t", 2 * time.Minute, true},
		{"zero seconds", "0", 0, true},
		{"future HTTP date", now.Add(3 * time.Minute).Format(http.TimeFormat), 3 * time.Minute, true},
		{"current HTTP date", now.Format(http.TimeFormat), 0, true},
		{"past HTTP date", now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"missing", "", 0, false},
		{"whitespace only", " \t", 0, false},
		{"negative seconds", "-1", 0, false},
		{"fractional seconds", "1.5", 0, false},
		{"malformed", "later", 0, false},
		{"multiple values", "30, 120", 0, false},
		{"overflow with invalid suffix", "9999999999999999999999999999999s", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delay, supported := ParseRetryAfter(tc.value, now)
			if delay != tc.delay || supported != tc.supported {
				t.Fatalf("retry signal %q returned %s supported=%t, want %s supported=%t", tc.value, delay, supported, tc.delay, tc.supported)
			}
			if legacy := RetryAfter(tc.value, now); legacy != delay {
				t.Fatalf("duration-only retry parser returned %s, validity-aware parser returned %s", legacy, delay)
			}
		})
	}
}
