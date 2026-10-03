package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

// reserved is what admission reserves for a request sent to a model.
func reserved(parsed *openai.Request, model string, defaults map[string]json.RawMessage) int64 {
	return estimate.Walk(parsed).Estimate(estimate.ForModel(model), defaults).Tokens()
}

// TestAdmissionEstimate pins the reservation estimate against the shapes a
// caller can ask for. Every expectation is a literal: an estimate recomputed
// the way the implementation computes it would assert nothing. The cases that
// name no model are sent to one the tokenizer registry does not know, which is
// charged four characters to a token; the ones that name an OpenAI model are
// counted by its tokenizer, with the numbers OpenAI's tiktoken gives and the
// framing the cookbook documents.
func TestAdmissionEstimate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family openai.Family
		model  string
		body   string
		want   int64
	}{
		{
			name:   "unbounded output is charged the default",
			family: openai.FamilyChat,
			// "hello" is five characters, so two tokens.
			body: `{"model":"model-a","messages":[{"role":"user","content":"hello"}]}`,
			want: 2 + estimate.DefaultOutputTokens,
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
			want:   1 + estimate.ImageTokens + 1,
		},
		{
			name:   "audio and files cost more than an image",
			family: openai.FamilyChat,
			body:   `{"model":"model-a","max_tokens":1,"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`,
			want:   estimate.MediaTokens + 1,
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
			want:   1 + estimate.ImageTokens + 5,
		},
		{
			name:   "an absurd request saturates instead of overflowing",
			family: openai.FamilyChat,
			body:   `{"model":"model-a","max_completion_tokens":9007199254740991,"n":1024,"messages":[{"role":"user","content":"hi"}]}`,
			want:   maxEstimate,
		},
		{
			name:   "an OpenAI model counts the text exactly and frames the message",
			family: openai.FamilyChat,
			model:  "gpt-4o",
			// "hello" and the role "user" are one token each, the message costs
			// three and the reply is primed with three.
			body: `{"model":"model-a","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			want: 1 + 1 + 3 + 3 + 10,
		},
		{
			name:   "the same request on a model without a public tokenizer",
			family: openai.FamilyChat,
			model:  "claude-sonnet-4-5",
			body:   `{"model":"model-a","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			want:   2 + 10,
		},
		{
			name:   "a script the four-character rule undercharges is counted exactly",
			family: openai.FamilyChat,
			model:  "gpt-4",
			// Five tokens in cl100k_base, against the one the heuristic charges.
			body: `{"model":"model-a","max_tokens":1,"messages":[{"role":"user","content":"日本語で"}]}`,
			want: 5 + 1 + 3 + 3 + 1,
		},
		{
			name:   "an image costs its flat charge next to exact text",
			family: openai.FamilyChat,
			model:  "gpt-4o",
			body:   `{"model":"model-a","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
			want:   1 + estimate.ImageTokens + 1 + 3 + 3 + 1,
		},
		{
			name:   "responses instructions and input are messages of their own",
			family: openai.FamilyResponses,
			model:  "gpt-4.1",
			body:   `{"model":"model-a","max_output_tokens":10,"instructions":"Be concise and kind.","input":"What is 2+2?"}`,
			// Seven and five tokens of text, "user" and "system", two messages
			// and the reply priming.
			want: 7 + 5 + 1 + 1 + 3 + 3 + 3 + 10,
		},
		{
			name:   "embeddings are counted by the model's own encoding",
			family: openai.FamilyEmbeddings,
			model:  "text-embedding-3-small",
			body:   `{"model":"model-a","input":["hello","日本語で"]}`,
			want:   1 + 5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := openai.Parse(tc.family, []byte(tc.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			model := tc.model
			if model == "" {
				model = modelA
			}
			if got := reserved(parsed, model, nil); got != tc.want {
				t.Fatalf("estimate = %d, want %d", got, tc.want)
			}
		})
	}
	if got := reserved(nil, modelA, nil); got != estimate.DefaultOutputTokens {
		t.Fatalf("estimate without a parsed request = %d, want %d", got, estimate.DefaultOutputTokens)
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
	got := reserved(parsed, modelA, nil)
	if want := int64(1 + estimate.ImageTokens + 4096); got != want {
		t.Fatalf("estimate = %d, want %d", got, want)
	}
	if len(body) < 1_000_000 || got > 100_000 {
		t.Fatalf("a %d byte request estimated %d tokens, which a generous key could not admit", len(body), got)
	}
}

func TestAdmissionRejectionMapping(t *testing.T) {
	const exhausted = "The API key cost budget was exhausted. Unpriced attempts accrue 0."
	for _, tc := range []struct {
		name       string
		dimension  limits.Dimension
		estimate   bool
		retryAfter time.Duration
		code       string
		message    string
		want       time.Duration
	}{
		{"requests", limits.DimensionRequests, false, 12 * time.Second, "rate_limit_exceeded", "requests per minute", 12 * time.Second},
		{"tokens", limits.DimensionTokens, false, 30 * time.Second, "rate_limit_exceeded", "tokens per minute", 30 * time.Second},
		{"concurrency", limits.DimensionConcurrency, false, time.Hour, "rate_limit_exceeded", "concurrency", maxConcurrencyRetryHint},
		// An exhausted budget answers with the one message the contract pins.
		{"daily exhausted", limits.DimensionDailyCost, false, 90 * time.Second, "budget_exhausted", exhausted, 90 * time.Second},
		{"monthly exhausted", limits.DimensionMonthlyCost, false, 0, "budget_exhausted", exhausted, time.Second},
		// A budget with room left says it is the request's estimate that does not
		// fit, under the same code, and never claims the budget is exhausted.
		{"daily estimate", limits.DimensionDailyCost, true, time.Second, "budget_exhausted", "cannot cover this request's estimated cost", time.Second},
		{"monthly estimate", limits.DimensionMonthlyCost, true, time.Hour, "budget_exhausted", "Lower max_tokens", time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := rateLimited(tc.dimension, tc.retryAfter, tc.estimate)
			if e.Status != 429 || e.Type != "rate_limit_error" || e.Code != tc.code {
				t.Fatalf("error = %d %s/%s, want 429 rate_limit_error/%s", e.Status, e.Type, e.Code, tc.code)
			}
			if !strings.Contains(e.Message, tc.message) {
				t.Fatalf("message %q does not mention %q", e.Message, tc.message)
			}
			if tc.estimate && strings.Contains(e.Message, "exhausted") {
				t.Fatalf("message %q calls a budget with room left exhausted", e.Message)
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

// TestOnlyTheKeysRequestAsksForTheAllowance proves the request that describes a
// key's budgets asks the rate script to state the allowance the response headers
// report, and the ones that describe a provider's connection or credential quota
// do not: they are reserved on every attempt and nothing reads what they have
// left.
func TestOnlyTheKeysRequestAsksForTheAllowance(t *testing.T) {
	rpm, tokens := int64(10), int64(100)
	key := keyRequest(admissionAuthority(access.KeyPolicy{RequestsPerMinute: &rpm, TokensPerMinute: &tokens}), 5, time.Second)
	if !key.ReportRate {
		t.Fatal("the key's request does not ask for its allowance")
	}
	provider := &runtime.Provider{ID: uuid.NewString(), Limits: &runtime.Limits{RequestsPerMinute: &rpm, TokensPerMinute: &tokens}}
	if connectionRequest(provider, 5, time.Second).ReportRate {
		t.Fatal("a provider connection's request asks for an allowance nothing reports")
	}
	slot := &runtime.Slot{ID: uuid.NewString(), RequestsPerMinute: &rpm, TokensPerMinute: &tokens}
	if slotRequest(slot, 5, time.Second).ReportRate {
		t.Fatal("a credential slot's request asks for an allowance nothing reports")
	}
}

func TestAdmissionWithoutLimiter(t *testing.T) {
	rpm := int64(10)
	cost := "5.00"
	unlimited := admissionAuthority(access.KeyPolicy{})
	throttled := admissionAuthority(access.KeyPolicy{RequestsPerMinute: &rpm})
	budgeted := admissionAuthority(access.KeyPolicy{DailyCostLimit: &cost})

	for _, a := range []*Admission{nil, NewAdmission(nil, func() limits.OutagePolicy { return limits.FailClosed }, slog.New(slog.DiscardHandler))} {
		lease, e := a.reserveKey(context.Background(), unlimited, "openai", 10, time.Second)
		if lease != nil || e != nil {
			t.Fatalf("key without hard limits: lease %v error %v", lease, e)
		}
		for _, authority := range []access.Authority{throttled, budgeted} {
			if _, e := a.reserveKey(context.Background(), authority, "openai", 10, time.Second); e == nil || e.Code != "distributed_limits_unavailable" {
				t.Fatalf("limited key admitted without a limiter: %v", e)
			}
		}
		if got := a.FailOpenTotal(); got != 0 {
			t.Fatalf("fail open total = %d, want 0", got)
		}
	}
}

func TestAdmissionFailsOpenOnlyForServiceOutages(t *testing.T) {
	open := NewAdmission(nil, func() limits.OutagePolicy { return limits.FailOpen }, slog.New(slog.DiscardHandler))
	rpm := int64(10)
	if _, e := open.reserveKey(context.Background(), admissionAuthority(access.KeyPolicy{RequestsPerMinute: &rpm}), "openai", 10, time.Second); e == nil {
		t.Fatal("no configured limiter is not an eligible fail-open outage")
	}
	for _, cause := range []error{
		limits.ErrMalformedState, limits.ErrUnexpectedResponse,
		&limits.InvalidRequestError{Reason: "invalid quota"},
	} {
		if e := open.outage("key", false, cause); e == nil || e.Status != 503 {
			t.Fatalf("semantic error admitted: %v", cause)
		}
	}
	outage := &limits.ServiceError{Err: errors.New("unreachable")}
	if e := open.outage("key", false, outage); e != nil {
		t.Fatalf("eligible outage failed closed: %v", e)
	}
	if e := open.outage("key", true, outage); e == nil || e.Status != 503 {
		t.Fatal("cost budget failed open")
	}
	if got := open.FailOpenTotal(); got != 1 {
		t.Fatalf("fail open total = %d, want 1", got)
	}
}

func TestSettleKeyWithoutLease(t *testing.T) {
	// Nothing to settle must stay silent rather than panic on a nil lease.
	settleKey(context.Background(), nil, true, nil, slog.New(slog.DiscardHandler))
	var reservation *targetReservation
	reservation.settle(context.Background(), false, nil)
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
