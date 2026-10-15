package gateway

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestEstimateUsesTheEffectiveProviderRequest(t *testing.T) {
	for _, tc := range []struct {
		name           string
		family         wire.Format
		body, defaults string
		want           int64
	}{
		{"default output and candidates", wire.Chat,
			`{"model":"m","messages":[{"role":"user","content":"abcd"}]}`,
			`{"max_tokens":100,"n":3}`, 301},
		{"explicit alias overrides default", wire.Chat,
			`{"model":"m","messages":[{"role":"user","content":""}],"max_completion_tokens":7}`,
			`{"max_tokens":100}`, 7},
		{"explicit null opts out of default", wire.Chat,
			`{"model":"m","messages":[{"role":"user","content":""}],"max_tokens":null}`,
			`{"max_completion_tokens":100}`, estimate.DefaultOutputTokens},
		{"responses instructions", wire.Responses,
			`{"model":"m","input":"abcd","instructions":"abcdefgh","max_output_tokens":7}`,
			`{}`, 10},
		{"default instructions", wire.Responses,
			`{"model":"m","input":"abcd"}`,
			`{"instructions":"abcdefgh","max_output_tokens":7}`, 10},
		{"null output is not one token", wire.Responses,
			`{"model":"m","input":"abcd","max_output_tokens":null}`,
			`{}`, 1 + estimate.DefaultOutputTokens},
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
			if _, err := openai.Encode(parsed, "upstream", defaults); err != nil {
				t.Fatal(err)
			}
			if got := reserved(parsed, modelA, defaults); got != tc.want {
				t.Fatalf("estimate = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestKeyEstimateCoversEveryCandidate(t *testing.T) {
	parsed, err := openai.Parse(wire.Chat,
		[]byte(`{"model":"m","messages":[{"role":"user","content":"abcd"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	x := &call{

		request: request{release: &runtime.Release{Snapshot: &routing.Snapshot{
			Providers: map[string]routing.Provider{
				"a": {ID: "a", ParameterDefaults: map[string]json.RawMessage{"max_tokens": json.RawMessage("10")}},
				"b": {ID: "b", ParameterDefaults: map[string]json.RawMessage{"max_tokens": json.RawMessage("100")}},
			},
		}}}, routeLeg: routeLeg{attempts: []routing.Attempt{{ProviderID: "a"}, {ProviderID: "b"}}}, generation: generationCall{parsed: parsed},
	}
	if got := requestEstimate(x); got != 101 {
		t.Fatalf("key estimate = %d, want the largest effective request", got)
	}
}

func TestPreparedRoutingKeepsTargetDefaultsDistinctThroughReservation(t *testing.T) {
	h := newHarness(t, Config{})
	var first routing.Provider
	for _, provider := range h.rt.release.Snapshot.Providers {
		if provider.Name == "a" {
			first = provider
			break
		}
	}
	first.ProfileID, first.ProfileRevision = "compatible-chat", "1"
	first.OperationDefaults = map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"max_tokens": json.RawMessage(`5000`)}}}
	second := first
	second.ID, second.RevisionID = "different-provider", "different-revision"
	second.OperationDefaults = map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"max_tokens": json.RawMessage(`9000`)}}}
	parsed, err := openai.Parse(wire.Chat, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"abcd"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	x := &call{

		request: request{release: &runtime.Release{Snapshot: &routing.Snapshot{Providers: map[string]routing.Provider{
			first.ID: first, second.ID: second,
		}}}}, routeLeg: routeLeg{attempts: []routing.Attempt{{ProviderID: first.ID, UpstreamModel: modelA}, {ProviderID: second.ID, UpstreamModel: modelA}}}, generation: generationCall{parsed: parsed},
	}
	for i, tc := range []struct {
		provider *routing.Provider
		bound    int64
	}{
		{&first, 5000}, {&second, 9000},
	} {
		prepared, err := x.preparedProvider(tc.provider, "", modelA)
		if err != nil {
			t.Fatal(err)
		}
		if prepared.demand.MaxOutputTokens == nil || *prepared.demand.MaxOutputTokens != tc.bound {
			t.Fatalf("provider %s routing bound = %v, want %d", tc.provider.ID, prepared.demand.MaxOutputTokens, tc.bound)
		}
		if got := x.attemptReservation(x.attempts[i], tc.provider); got != tc.bound+1 {
			t.Fatalf("provider %s reservation = %d, want %d", tc.provider.ID, got, tc.bound+1)
		}
	}
	if got := requestEstimate(x); got != 9001 {
		t.Fatalf("failover reservation = %d, want the largest target bound", got)
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
	plan, err := routing.Select(snapshot, routeSlug, operationGeneration, surfaceOpenAI, "unary", []byte(h.keyID), routing.Options{})
	if err != nil {
		t.Fatal(err)
	}
	x := &call{
		request: request{release: h.rt.release},
		keyID:   h.keyID, routeLeg: routeLeg{route: &route,
			attempts: plan.Attempts[:1],
			budget:   3},
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
	x := &call{estimate: 100, facts: []AttemptFact{
		{Usage: &wire.Usage{InputTokens: 2, OutputTokens: 3}, UsageObserved: true},
		{BillingUncertain: true},
		{UsageComplete: true}, // an explicit rejection costs no tokens
		{Usage: &wire.Usage{InputTokens: 7, OutputTokens: 5}, UsageObserved: true},
	}}
	if got := *x.settledTokens(); got != 117 {
		t.Fatalf("settled = %d, want 5 + 100 + 12", got)
	}
}

func TestTokenSettlementSaturatesBeforeAdding(t *testing.T) {
	huge := &wire.Usage{InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64}
	if got := totalTokens(huge); got == nil || *got != maxEstimate {
		t.Fatalf("overflowing usage = %v, want saturation", got)
	}
	if got := totalTokens(&wire.Usage{InputTokens: 7, OutputTokens: 5, TotalTokens: 1}); got == nil || *got != 12 {
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

func TestPartialTokenSettlementPreservesOrIncreasesTheReservation(t *testing.T) {
	for _, tc := range []struct {
		name string
		fact AttemptFact
		want int64
	}{
		{"partial below estimate", AttemptFact{Usage: &wire.Usage{InputTokens: 11, OutputTokens: 1}, UsageObserved: true, BillingUncertain: true}, 100},
		{"partial above estimate", AttemptFact{Usage: &wire.Usage{InputTokens: 150, OutputTokens: 3}, UsageObserved: true, BillingUncertain: true}, 153},
		{"complete observed", AttemptFact{Usage: &wire.Usage{InputTokens: 11, OutputTokens: 1}, UsageObserved: true, UsageComplete: true}, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actual := settledAttemptTokens(&tc.fact, 100)
			if actual == nil || *actual != tc.want {
				t.Fatalf("target settlement=%v want%d", actual, tc.want)
			}
			x := call{estimate: 100, facts: []AttemptFact{tc.fact}}
			if got := *x.settledTokens(); got != tc.want {
				t.Fatalf("key settlement=%d want%d", got, tc.want)
			}
		})
	}
	x := call{estimate: 100, facts: []AttemptFact{
		{Usage: &wire.Usage{InputTokens: 11, OutputTokens: 1}, UsageObserved: true, BillingUncertain: true},
		{Usage: &wire.Usage{InputTokens: 150, OutputTokens: 3}, UsageObserved: true, BillingUncertain: true},
		{Usage: &wire.Usage{InputTokens: 11, OutputTokens: 1}, UsageObserved: true, UsageComplete: true},
		{BillingUncertain: true}, {UsageComplete: true},
	}}
	if got := *x.settledTokens(); got != 365 {
		t.Fatalf("all attempts=%d want365", got)
	}
}
