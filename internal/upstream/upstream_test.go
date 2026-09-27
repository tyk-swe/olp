package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptrace"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestAcceptanceFollowsExplicitEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		evidence Evidence
		want     Acceptance
	}{
		{"nothing written", Evidence{}, NotSent},
		{"request reached the upstream", Evidence{Reached: true}, Unknown},
		{"success began", Evidence{Reached: true, Accepted: true}, Accepted},
		{"result complete", Evidence{Reached: true, Accepted: true, Settled: true}, Terminal},
		{"stated rejection", Evidence{Reached: true, Status: 400}, Terminal},
		{"rate limit", Evidence{Reached: true, Status: 429}, Terminal},
		{"server failure leaves work possible", Evidence{Reached: true, Status: 503}, Unknown},
		{"a status implies the request arrived", Evidence{Status: 401}, Terminal},
		{"in-band error keeps the response accepted", Evidence{Reached: true, Accepted: true, Error: &openai.UpstreamError{Type: "overloaded_error"}}, Accepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.evidence.Acceptance(); got != tc.want {
				t.Fatalf("acceptance %q, want %q", got, tc.want)
			}
		})
	}
	for acceptance, unresolved := range map[Acceptance]bool{NotSent: false, Unknown: true, Accepted: true, Terminal: false} {
		if acceptance.Unresolved() != unresolved {
			t.Errorf("%q unresolved %v, want %v", acceptance, !unresolved, unresolved)
		}
	}
}

func TestClassifyStatusRejections(t *testing.T) {
	contextCode := &openai.UpstreamError{Type: "invalid_request_error", Code: "context_length_exceeded"}
	for _, tc := range []struct {
		name       string
		classifier Classifier
		status     int
		stated     *openai.UpstreamError
		want       Class
	}{
		{"unauthorized", Classifier{}, 401, nil, Credential},
		{"forbidden", Classifier{}, 403, nil, Credential},
		{"rate limited", Classifier{}, 429, nil, RateLimit},
		{"server failure", Classifier{}, 500, nil, ServerError},
		{"gateway failure", Classifier{}, 504, nil, ServerError},
		{"bad request", Classifier{}, 400, &openai.UpstreamError{Type: "invalid_request_error"}, ClientError},
		{"not found", Classifier{}, 404, nil, ClientError},
		{"unexpected success status", Classifier{}, 201, nil, ClientError},
		{"context rejection that can fail over", Classifier{ContextWindow: true}, 400, contextCode, ContextWindow},
		{"context rejection of a call without a window", Classifier{}, 400, contextCode, ClientError},
		{"status outranks a context code", Classifier{ContextWindow: true}, 429, contextCode, RateLimit},
		{"server failure outranks a context code", Classifier{ContextWindow: true}, 500, contextCode, ServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.classifier.Classify(Evidence{Reached: true, Status: tc.status, Error: tc.stated})
			if got.Class != tc.want {
				t.Fatalf("class %q, want %q", got.Class, tc.want)
			}
		})
	}
}

func TestContextRejectionMatchesTypedCodesExactly(t *testing.T) {
	windowed := Classifier{ContextWindow: true}
	for code, want := range map[string]bool{
		"context_length_exceeded":     true,
		"Context-Length-Exceeded":     true,
		"context window exceeded":     true,
		"max_context_length_exceeded": true,
		"prompt_too_long":             true,
		"invalid_value":               false,
		"context_length":              false,
		"":                            false,
	} {
		got := windowed.Classify(Evidence{Status: 400, Error: &openai.UpstreamError{Code: code}}).Class == ContextWindow
		if got != want {
			t.Errorf("code %q: context window %v, want %v", code, got, want)
		}
	}
	if windowed.Classify(Evidence{Status: 400, Error: &openai.UpstreamError{Type: "context_window_exceeded"}}).Class != ContextWindow {
		t.Error("typed error type must classify")
	}
	if windowed.Classify(Evidence{Status: 400, Error: &openai.UpstreamError{Message: "context_length_exceeded"}}).Class != ClientError {
		t.Error("message prose must never classify")
	}
	if windowed.Classify(Evidence{Status: 400}).Class != ClientError {
		t.Error("a rejection without an error body must not classify as a context rejection")
	}
}

func TestClassifyInBandErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		classifier Classifier
		stated     openai.UpstreamError
		committed  bool
		want       Class
	}{
		{"context rejection", Classifier{ContextWindow: true}, openai.UpstreamError{Code: "context_window_exceeded"}, false, ContextWindow},
		{"context rejection without a window", Classifier{}, openai.UpstreamError{Code: "context_window_exceeded"}, false, ClientError},
		{"authentication type", Classifier{}, openai.UpstreamError{Type: "authentication_error"}, false, Credential},
		{"permission status code", Classifier{}, openai.UpstreamError{Code: "403"}, false, Credential},
		{"rate limit type", Classifier{}, openai.UpstreamError{Type: "rate_limit_error"}, false, RateLimit},
		{"exhausted resource", Classifier{}, openai.UpstreamError{Code: "RESOURCE_EXHAUSTED"}, false, RateLimit},
		{"throttling", Classifier{}, openai.UpstreamError{Type: "ThrottlingException"}, false, RateLimit},
		{"deadline", Classifier{}, openai.UpstreamError{Code: "DEADLINE_EXCEEDED"}, false, Timeout},
		{"invalid request", Classifier{}, openai.UpstreamError{Type: "invalid_request_error"}, true, ClientError},
		{"validation", Classifier{}, openai.UpstreamError{Type: "ValidationException"}, false, ClientError},
		{"unknown failure before commit", Classifier{}, openai.UpstreamError{Type: "overloaded_error"}, false, ServerError},
		{"unknown failure after commit", Classifier{}, openai.UpstreamError{Type: "overloaded_error"}, true, Protocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stated := tc.stated
			got := tc.classifier.Classify(Evidence{Reached: true, Accepted: true, Error: &stated, Committed: tc.committed})
			if got.Class != tc.want || got.Acceptance != Accepted {
				t.Fatalf("outcome %+v, want class %q accepted", got, tc.want)
			}
		})
	}
}

func TestDeclaredClassificationPrecedesTheBuiltInRules(t *testing.T) {
	declared := Classifier{ContextWindow: true, Declared: []Rule{
		{Status: 400, Code: "insufficient_quota", Class: RateLimit},
		{Status: 400, Type: "billing_error", Class: Credential},
		{Status: 503, Class: ClientError},
		{Code: "account_suspended", Class: Credential},
		{Type: "quota_exceeded", Class: RateLimit},
		{Status: 400, Type: "busy", Class: ServerError},
		{Code: "context_length_exceeded", Class: ClientError},
		{Status: 409, Code: "busy", Class: ServerError},
	}}
	stated := func(errorType, code string) *openai.UpstreamError {
		return &openai.UpstreamError{Type: errorType, Code: code, Message: "declared"}
	}
	for _, tc := range []struct {
		name     string
		evidence Evidence
		want     Class
	}{
		{"declared status and code", Evidence{Reached: true, Status: 400, Error: stated("invalid_request_error", "insufficient_quota")}, RateLimit},
		{"declared status and type", Evidence{Reached: true, Status: 400, Error: stated("billing_error", "")}, Credential},
		{"declared status alone", Evidence{Reached: true, Status: 503}, ClientError},
		{"code declared for any status", Evidence{Reached: true, Status: 429, Error: stated("", "account_suspended")}, Credential},
		{"code declared for in-band errors too", Evidence{Reached: true, Accepted: true, Error: stated("", "account_suspended")}, Credential},
		{"declared in-band type", Evidence{Reached: true, Accepted: true, Error: stated("quota_exceeded", "")}, RateLimit},
		{"declared in-band type after commit", Evidence{Reached: true, Accepted: true, Committed: true, Error: stated("quota_exceeded", "")}, RateLimit},
		{"a status rule never matches in-band", Evidence{Reached: true, Accepted: true, Error: stated("busy", "")}, ServerError},
		{"declaration outranks a context rejection", Evidence{Reached: true, Status: 400, Error: stated("", "context_length_exceeded")}, ClientError},
		{"values match exactly", Evidence{Reached: true, Status: 400, Error: stated("invalid_request_error", "Insufficient_Quota")}, ClientError},
		{"undeclared code of a declared status", Evidence{Reached: true, Status: 400, Error: stated("invalid_request_error", "invalid_value")}, ClientError},
		{"undeclared status", Evidence{Reached: true, Status: 401}, Credential},
		{"undeclared in-band error", Evidence{Reached: true, Accepted: true, Error: stated("rate_limit_error", "")}, RateLimit},
		{"a code rule needs a stated error", Evidence{Reached: true, Status: 409}, ClientError},
		{"interruption outranks a declaration", Evidence{Reached: true, Status: 503, Interrupted: context.Canceled}, Cancelled},
		{"transport failures state nothing", Evidence{Reached: true, Err: errors.New("connection reset")}, Connect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := declared.Classify(tc.evidence); got.Class != tc.want || got.Acceptance != tc.evidence.Acceptance() {
				t.Fatalf("outcome %+v, want class %q", got, tc.want)
			}
		})
	}
	first := Classifier{Declared: []Rule{{Status: 400, Code: "busy", Class: Credential}, {Status: 400, Class: RateLimit}}}
	if got := first.Classify(Evidence{Status: 400, Error: stated("", "busy")}).Class; got != Credential {
		t.Fatalf("the first matching rule did not decide: %q", got)
	}
	if got := first.Classify(Evidence{Status: 400, Error: stated("", "other")}).Class; got != RateLimit {
		t.Fatalf("a later matching rule did not decide: %q", got)
	}
	once := Classifier{AtMostOnce: true, Declared: []Rule{{Status: 400, Class: ServerError}, {Type: "busy", Class: ServerError}}}
	if got := once.Classify(Evidence{Reached: true, Status: 400}); got != (Outcome{ServerError, Terminal}) {
		t.Fatalf("a retryable rejection the upstream settled %+v", got)
	}
	if got := once.Classify(Evidence{Reached: true, Accepted: true, Error: stated("busy", "")}); got != (Outcome{Ambiguous, Accepted}) {
		t.Fatalf("a retryable in-band failure of accepted work %+v", got)
	}
}

func TestClassifyInterruptionsAndTransportFailures(t *testing.T) {
	malformed := &openai.ProtocolError{Detail: "malformed event"}
	for _, tc := range []struct {
		name     string
		evidence Evidence
		want     Class
	}{
		{"caller cancelled", Evidence{Reached: true, Interrupted: context.Canceled, Err: errors.New("broken pipe")}, Cancelled},
		{"deadline expired", Evidence{Reached: true, Interrupted: context.DeadlineExceeded}, Timeout},
		{"wrapped deadline", Evidence{Interrupted: fmt.Errorf("attempt: %w", context.DeadlineExceeded)}, Timeout},
		{"interruption outranks a status", Evidence{Reached: true, Status: 401, Interrupted: context.Canceled}, Cancelled},
		{"interruption after commit", Evidence{Reached: true, Accepted: true, Committed: true, Interrupted: context.Canceled}, Cancelled},
		{"connection refused", Evidence{Err: errors.New("connection refused")}, Connect},
		{"connection lost after commit", Evidence{Reached: true, Accepted: true, Committed: true, Err: errors.New("unexpected EOF")}, Protocol},
		{"malformed stream", Evidence{Reached: true, Accepted: true, Err: fmt.Errorf("decode: %w", malformed)}, Protocol},
		{"oversized event", Evidence{Reached: true, Accepted: true, Err: openai.ErrEventTooLarge}, Protocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Classifier{}).Classify(tc.evidence).Class; got != tc.want {
				t.Fatalf("class %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAtMostOnceCallsNeverFailOverUnresolvedWork(t *testing.T) {
	once := Classifier{ContextWindow: true, AtMostOnce: true}
	for _, tc := range []struct {
		name     string
		evidence Evidence
		want     Outcome
	}{
		{"unsent connect failure fails over", Evidence{Err: errors.New("dial")}, Outcome{Connect, NotSent}},
		{"sent connect failure", Evidence{Reached: true, Err: errors.New("reset")}, Outcome{Ambiguous, Unknown}},
		{"timeout after acceptance", Evidence{Reached: true, Accepted: true, Interrupted: context.DeadlineExceeded}, Outcome{Ambiguous, Accepted}},
		{"server failure", Evidence{Reached: true, Status: 502}, Outcome{Ambiguous, Unknown}},
		{"in-band server failure", Evidence{Reached: true, Accepted: true, Error: &openai.UpstreamError{Type: "overloaded_error"}}, Outcome{Ambiguous, Accepted}},
		{"stated rate limit", Evidence{Reached: true, Status: 429}, Outcome{RateLimit, Terminal}},
		{"stated credential rejection", Evidence{Reached: true, Status: 401}, Outcome{Credential, Terminal}},
		{"context rejection", Evidence{Reached: true, Status: 400, Error: &openai.UpstreamError{Code: "prompt_too_long"}}, Outcome{ContextWindow, Terminal}},
		{"in-band rate limit", Evidence{Reached: true, Accepted: true, Error: &openai.UpstreamError{Type: "rate_limit_error"}}, Outcome{RateLimit, Accepted}},
		{"caller cancellation", Evidence{Reached: true, Interrupted: context.Canceled}, Outcome{Cancelled, Unknown}},
		{"malformed accepted result", Evidence{Reached: true, Accepted: true, Err: &openai.ProtocolError{}}, Outcome{Protocol, Accepted}},
		{"failure after the result settled", Evidence{Reached: true, Accepted: true, Settled: true, Err: errors.New("commit")}, Outcome{Connect, Terminal}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := once.Classify(tc.evidence); got != tc.want {
				t.Fatalf("outcome %+v, want %+v", got, tc.want)
			}
		})
	}
	if got := (Classifier{}).Classify(Evidence{Reached: true, Status: 502}); got != (Outcome{ServerError, Unknown}) {
		t.Fatalf("repeatable server failure %+v", got)
	}
}

func TestTraceSuppliesReachedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event func(*httptrace.ClientTrace)
		want  bool
	}{
		{"headers written", func(trace *httptrace.ClientTrace) { trace.WroteHeaders() }, true},
		{"request written", func(trace *httptrace.ClientTrace) { trace.WroteRequest(httptrace.WroteRequestInfo{}) }, true},
		{"request write failed", func(trace *httptrace.ClientTrace) {
			trace.WroteRequest(httptrace.WroteRequestInfo{Err: errors.New("closed")})
		}, false},
		{"response began", func(trace *httptrace.ClientTrace) { trace.GotFirstResponseByte() }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reached atomic.Bool
			tc.event(Trace(&reached))
			if reached.Load() != tc.want {
				t.Fatalf("reached %v, want %v", reached.Load(), tc.want)
			}
		})
	}
}
