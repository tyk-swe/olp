package upstream

import (
	"testing"

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
