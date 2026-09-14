package gateway

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// TestAdmissionEstimate pins the reservation estimate against the shapes a
// caller can ask for. Every expectation is a literal: an estimate recomputed
// the way the implementation computes it would assert nothing.
func TestAdmissionEstimate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family openai.Family
		body   string
		want   int64
	}{
		{
			name:   "unbounded output is charged the default",
			family: openai.FamilyChat,
			// "hello" is five characters, so two tokens.
			body: `{"model":"model-a","messages":[{"role":"user","content":"hello"}]}`,
			want: 2 + defaultOutputTokens,
		},
		{
			name:   "max_tokens bounds the reply",
			family: openai.FamilyChat,
			body:   `{"model":"model-a","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			want:   2 + 10,
		},
		{
			name:   "every candidate is charged",
			family: openai.FamilyChat,
			body:   `{"model":"model-a","max_completion_tokens":7,"n":3,"messages":[{"role":"user","content":"hi"}]}`,
			want:   1 + 7*3,
		},
		{
			name:   "characters are counted, not bytes",
			family: openai.FamilyChat,
			// Four three-byte characters are one token, not three.
			body: `{"model":"model-a","max_tokens":1,"messages":[{"role":"user","content":"日本語で"}]}`,
			want: 1 + 1,
		},
		{
			name:   "an image part costs a flat charge",
			family: openai.FamilyChat,
			body:   `{"model":"model-a","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAAAAAAAAAAAAAA"}}]}]}`,
			want:   1 + imageTokens + 1,
		},
		{
			name:   "audio and files cost more than an image",
			family: openai.FamilyChat,
			body:   `{"model":"model-a","max_tokens":1,"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`,
			want:   mediaTokens + 1,
		},
		{
			name:   "tool calls and their results are prompt text",
			family: openai.FamilyChat,
			// The sender name "abcd" is one token, the call name "lookup" two,
			// its arguments three, the call id "call_1" two, and the result
			// "ok" one.
			body: `{"model":"model-a","max_tokens":1,"messages":[{"role":"assistant","content":null,"name":"abcd","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`,
			want: 1 + 2 + 3 + 2 + 1 + 1,
		},
		{
			name:   "a tool catalogue is charged with its schema",
			family: openai.FamilyChat,
			// "lookup" is two tokens and "Look it up" three; the schema is
			// charged as the seventeen characters it compacts to, so five.
			body: `{"model":"model-a","max_tokens":1,"messages":[{"role":"user","content":""}],"tools":[{"type":"function","function":{"name":"lookup","description":"Look it up","parameters": {"type": "object"}}}]}`,
			want: 2 + 3 + 5 + 1,
		},
		{
			name:   "responses carries its own bound",
			family: openai.FamilyResponses,
			body:   `{"model":"model-a","max_output_tokens":25,"input":"hello"}`,
			want:   2 + 25,
		},
		{
			name:   "responses input items are walked like messages",
			family: openai.FamilyResponses,
			body:   `{"model":"model-a","max_output_tokens":5,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`,
			want:   1 + imageTokens + 5,
		},
		{
			name:   "an absurd request saturates instead of overflowing",
			family: openai.FamilyChat,
			body:   `{"model":"model-a","max_completion_tokens":9007199254740991,"n":1024,"messages":[{"role":"user","content":"hi"}]}`,
			want:   maxEstimate,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := openai.Parse(tc.family, []byte(tc.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := estimateTokens(parsed); got != tc.want {
				t.Fatalf("estimate = %d, want %d", got, tc.want)
			}
		})
	}
	if got := estimateTokens(nil); got != defaultOutputTokens {
		t.Fatalf("estimate without a parsed request = %d, want %d", got, defaultOutputTokens)
	}
}

// TestAdmissionEstimateIgnoresInlineMediaSize keeps an inline image at the
// flat charge. A megabyte of base64 is one image to the provider, and charging
// the bytes it occupies would refuse — before any attempt, with no window that
// could ever hold it — a request any generous token limit should admit.
func TestAdmissionEstimateIgnoresInlineMediaSize(t *testing.T) {
	body := `{"model":"model-a","max_tokens":4096,"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,` +
		strings.Repeat("A", 1_000_000) + `"}}]}]}`
	parsed, err := openai.Parse(openai.FamilyChat, []byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := estimateTokens(parsed)
	if want := int64(1 + imageTokens + 4096); got != want {
		t.Fatalf("estimate = %d, want %d", got, want)
	}
	if len(body) < 1_000_000 || got > 100_000 {
		t.Fatalf("a %d byte request estimated %d tokens, which a generous key could not admit", len(body), got)
	}
}

func TestAdmissionRejectionMapping(t *testing.T) {
	for _, tc := range []struct {
		dimension  limits.Dimension
		retryAfter time.Duration
		code       string
		message    string
		want       time.Duration
	}{
		{limits.DimensionRequests, 12 * time.Second, "rate_limit_exceeded", "requests per minute", 12 * time.Second},
		{limits.DimensionTokens, 30 * time.Second, "rate_limit_exceeded", "tokens per minute", 30 * time.Second},
		{limits.DimensionConcurrency, time.Hour, "rate_limit_exceeded", "concurrency", maxConcurrencyRetryHint},
		// Both cost budgets answer with the one message the contract pins.
		{limits.DimensionDailyCost, 90 * time.Second, "budget_exhausted", "The API key cost budget was exhausted. Unpriced attempts accrue 0.", 90 * time.Second},
		{limits.DimensionMonthlyCost, 0, "budget_exhausted", "The API key cost budget was exhausted. Unpriced attempts accrue 0.", time.Second},
	} {
		t.Run(string(tc.dimension), func(t *testing.T) {
			e := rateLimited(tc.dimension, tc.retryAfter)
			if e.Status != 429 || e.Type != "rate_limit_error" || e.Code != tc.code {
				t.Fatalf("error = %d %s/%s, want 429 rate_limit_error/%s", e.Status, e.Type, e.Code, tc.code)
			}
			if !strings.Contains(e.Message, tc.message) {
				t.Fatalf("message %q does not mention %q", e.Message, tc.message)
			}
			if e.RetryAfter != tc.want {
				t.Fatalf("retry after = %s, want %s", e.RetryAfter, tc.want)
			}
		})
	}
}

// admissionAuthority builds a key with the limits a case needs.
func admissionAuthority(policy access.KeyPolicy) access.Authority {
	return access.Authority{ID: uuid.Must(uuid.NewV7()).String(), LookupID: "lookupid0123", Policy: policy}
}

func TestAdmissionWithoutLimiter(t *testing.T) {
	rpm := int64(10)
	cost := "5.00"
	unlimited := admissionAuthority(access.KeyPolicy{})
	throttled := admissionAuthority(access.KeyPolicy{RequestsPerMinute: &rpm})
	budgeted := admissionAuthority(access.KeyPolicy{DailyCostLimit: &cost})

	for _, a := range []*Admission{nil, NewAdmission(nil, func() limits.OutagePolicy { return limits.FailClosed }, slog.New(slog.DiscardHandler))} {
		lease, e := a.reserveKey(context.Background(), unlimited, 10, time.Second)
		if lease != nil || e != nil {
			t.Fatalf("key without hard limits: lease %v error %v", lease, e)
		}
		for _, authority := range []access.Authority{throttled, budgeted} {
			if _, e := a.reserveKey(context.Background(), authority, 10, time.Second); e == nil || e.Code != "distributed_limits_unavailable" {
				t.Fatalf("limited key admitted without a limiter: %v", e)
			}
		}
		if got := a.FailOpenTotal(); got != 0 {
			t.Fatalf("fail open total = %d, want 0", got)
		}
	}
}

func TestAdmissionFailsOpenOnlyForRateLimits(t *testing.T) {
	rpm, cost := int64(10), "5.00"
	open := NewAdmission(nil, func() limits.OutagePolicy { return limits.FailOpen }, slog.New(slog.DiscardHandler))
	if lease, e := open.reserveKey(context.Background(), admissionAuthority(access.KeyPolicy{RequestsPerMinute: &rpm}), 10, time.Second); lease != nil || e != nil {
		t.Fatalf("rate limited key not admitted: lease %v error %v", lease, e)
	}
	if got := open.FailOpenTotal(); got != 1 {
		t.Fatalf("fail open total = %d, want 1", got)
	}
	budgeted := admissionAuthority(access.KeyPolicy{RequestsPerMinute: &rpm, MonthlyCostLimit: &cost})
	if _, e := open.reserveKey(context.Background(), budgeted, 10, time.Second); e == nil || e.Status != 503 {
		t.Fatalf("cost budget admitted while the limiter was down: %v", e)
	}
	if got := open.FailOpenTotal(); got != 1 {
		t.Fatalf("fail open total = %d, want 1 after a budgeted key failed closed", got)
	}
}

func TestSettleKeyWithoutLease(t *testing.T) {
	// Nothing to settle must stay silent rather than panic on a nil lease.
	settleKey(context.Background(), nil, true, nil, slog.New(slog.DiscardHandler))
	var reservation *targetReservation
	reservation.settle(context.Background(), nil)
}

func TestTotalTokens(t *testing.T) {
	if got := totalTokens(nil); got != nil {
		t.Fatalf("usage-less total = %v, want nil", got)
	}
	for _, tc := range []struct {
		name  string
		usage openai.Usage
		want  int64
	}{
		{"reported total wins", openai.Usage{InputTokens: 4, OutputTokens: 6, TotalTokens: 10}, 10},
		{"halves are summed when no total is reported", openai.Usage{InputTokens: 4, OutputTokens: 6}, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := totalTokens(&tc.usage)
			if got == nil || *got != tc.want {
				t.Fatalf("total = %v, want %d", got, tc.want)
			}
		})
	}
}
