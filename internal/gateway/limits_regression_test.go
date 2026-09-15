package gateway

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestEstimateUsesTheEffectiveProviderRequest(t *testing.T) {
	for _, tc := range []struct {
		name           string
		family         openai.Family
		body, defaults string
		want           int64
	}{
		{"default output and candidates", openai.FamilyChat,
			`{"model":"m","messages":[{"role":"user","content":"abcd"}]}`,
			`{"max_tokens":100,"n":3}`, 301},
		{"explicit alias overrides default", openai.FamilyChat,
			`{"model":"m","messages":[{"role":"user","content":""}],"max_completion_tokens":7}`,
			`{"max_tokens":100}`, 7},
		{"explicit null opts out of default", openai.FamilyChat,
			`{"model":"m","messages":[{"role":"user","content":""}],"max_tokens":null}`,
			`{"max_completion_tokens":100}`, defaultOutputTokens},
		{"responses instructions", openai.FamilyResponses,
			`{"model":"m","input":"abcd","instructions":"abcdefgh","max_output_tokens":7}`,
			`{}`, 10},
		{"default instructions", openai.FamilyResponses,
			`{"model":"m","input":"abcd"}`,
			`{"instructions":"abcdefgh","max_output_tokens":7}`, 10},
		{"null output is not one token", openai.FamilyResponses,
			`{"model":"m","input":"abcd","max_output_tokens":null}`,
			`{}`, 1 + defaultOutputTokens},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := openai.Parse(tc.family, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			var defaults map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.defaults), &defaults); err != nil {
				t.Fatal(err)
			}
			if _, err := parsed.Encode("upstream", defaults); err != nil {
				t.Fatal(err)
			}
			if got := estimateTokens(parsed, defaults); got != tc.want {
				t.Fatalf("estimate = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestKeyEstimateCoversEveryCandidate(t *testing.T) {
	parsed, err := openai.Parse(openai.FamilyChat,
		[]byte(`{"model":"m","messages":[{"role":"user","content":"abcd"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	x := &execution{
		parsed: parsed,
		request: request{release: &runtime.Release{Snapshot: &runtime.Snapshot{
			Providers: map[string]runtime.Provider{
				"a": {ParameterDefaults: map[string]json.RawMessage{"max_tokens": json.RawMessage("10")}},
				"b": {ParameterDefaults: map[string]json.RawMessage{"max_tokens": json.RawMessage("100")}},
			},
		}}},
		attempts: []runtime.Attempt{{ProviderID: "a"}, {ProviderID: "b"}},
	}
	if got := requestEstimate(x); got != 101 {
		t.Fatalf("key estimate = %d, want the largest effective request", got)
	}
}

func TestKeyReservationCoversEveryAllowedAttempt(t *testing.T) {
	if got := keyReservationEstimate(101, 3); got != 303 {
		t.Fatalf("key reservation = %d, want one estimate per allowed attempt", got)
	}
	if got := keyReservationEstimate(101, 0); got != 101 {
		t.Fatalf("key reservation = %d, want a valid minimum reservation", got)
	}
	if got := keyReservationEstimate(maxEstimate, 2); got != maxEstimate {
		t.Fatalf("overflowing key reservation = %d, want saturation", got)
	}
}

func TestKeyReservationCapsAtDispatchableAttempts(t *testing.T) {
	h := newHarness(t, Config{})
	snapshot := h.rt.release.Snapshot
	route := snapshot.Routes[routeSlug]
	attempts, err := runtime.Select(snapshot, routeSlug, operationGeneration, surfaceOpenAI, "unary", []byte(h.keyID))
	if err != nil {
		t.Fatal(err)
	}
	x := &execution{
		request:  request{release: h.rt.release},
		keyID:    h.keyID,
		route:    &route,
		attempts: attempts[:1],
		budget:   3,
	}
	got := h.gateway.dispatchableAttempts(x)
	if got != 1 {
		t.Fatalf("dispatchable attempts = %d, want the one target/credential candidate", got)
	}
	if reservation := keyReservationEstimate(101, got); reservation != 101 {
		t.Fatalf("key reservation = %d, want one candidate estimate", reservation)
	}
}

func TestKeySettlementPreservesAllAttemptUsage(t *testing.T) {
	x := &execution{estimate: 100, facts: []AttemptFact{
		{Usage: &openai.Usage{InputTokens: 2, OutputTokens: 3}, UsageObserved: true},
		{BillingUncertain: true},
		{UsageComplete: true}, // an explicit rejection costs no tokens
		{Usage: &openai.Usage{InputTokens: 7, OutputTokens: 5}, UsageObserved: true},
	}}
	if got := *x.settledTokens(); got != 117 {
		t.Fatalf("settled = %d, want 5 + 100 + 12", got)
	}
}

func TestTokenSettlementSaturatesBeforeAdding(t *testing.T) {
	huge := &openai.Usage{InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64}
	if got := totalTokens(huge); got == nil || *got != maxEstimate {
		t.Fatalf("overflowing usage = %v, want saturation", got)
	}
	if got := totalTokens(&openai.Usage{InputTokens: 7, OutputTokens: 5, TotalTokens: 1}); got == nil || *got != 12 {
		t.Fatalf("contradictory total = %v, want at least the reported components", got)
	}
}

func TestPartialUpstreamWriteIsNotRefundable(t *testing.T) {
	state := &attemptState{}
	state.trace().WroteHeaders()
	failure := &attemptFailure{class: classConnect, dispatched: state.dispatched.Load()}
	if !failure.billingUncertain() {
		t.Fatal("a request partially sent upstream was treated as undispatched")
	}
}
