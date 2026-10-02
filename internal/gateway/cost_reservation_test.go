package gateway

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func costText(value string) *string { return &value }

func costCount(value int64) *int64 { return &value }

// costPrice prices a million tokens of input at one unit and of output at ten.
func costPrice() *usage.RoutingPrice {
	return &usage.RoutingPrice{Price: usage.Price{
		InputPerMillion: costText("1"), OutputPerMillion: costText("10"),
	}}
}

// costExecution is a request routed to the given attempts, each served by a
// provider with one usable slot, so the walk of dispatchable attempts has
// something to walk.
func costExecution(t *testing.T, body string, family openai.Family, budget int, attempts ...runtime.Attempt) *execution {
	t.Helper()
	request, err := openai.Parse(family, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &runtime.Snapshot{Providers: map[string]runtime.Provider{}}
	for index := range attempts {
		id := uuid.NewString()
		attempts[index].ProviderID, attempts[index].TargetID = id, uuid.NewString()
		if attempts[index].UpstreamModel == "" {
			attempts[index].UpstreamModel = "model-a"
		}
		snapshot.Providers[id] = runtime.Provider{
			ID: id, AuthMode: "none", Slots: []runtime.Slot{{ID: uuid.NewString(), Enabled: true}},
		}
	}
	return &execution{
		parsed: request, family: family, route: &runtime.Route{Slug: "team-chat", Fidelity: runtime.RouteFidelity{Mode: runtime.FidelityTransformed}},
		attempts: attempts, budget: budget, historicalSnapshot: snapshot,
	}
}

func budgeted() access.Authority {
	return admissionAuthority(access.KeyPolicy{DailyCostLimit: costText("5.00")})
}

func TestCostReservationIsTheMostAnAttemptCouldCost(t *testing.T) {
	const prompt = `[{"role":"user","content":"hello"}]`
	for _, tc := range []struct {
		name     string
		body     string
		family   openai.Family
		attempts []runtime.Attempt
		budget   int
		want     string
	}{
		{
			// Two input tokens at 1 per million and the default 4096 reply tokens at
			// 10 per million, each division rounded up.
			name: "no max_tokens reserves the default reply", family: openai.FamilyChat, budget: 1,
			body:     `{"model":"team-chat","messages":` + prompt + `}`,
			attempts: []runtime.Attempt{{Price: costPrice()}},
			want:     "0.040962",
		},
		{
			name: "max_tokens bounds the reply", family: openai.FamilyChat, budget: 1,
			body:     `{"model":"team-chat","max_tokens":16,"messages":` + prompt + `}`,
			attempts: []runtime.Attempt{{Price: costPrice()}},
			want:     "0.000162",
		},
		{
			name: "every candidate is priced", family: openai.FamilyChat, budget: 1,
			body:     `{"model":"team-chat","max_tokens":16,"n":2,"messages":` + prompt + `}`,
			attempts: []runtime.Attempt{{Price: costPrice()}},
			want:     "0.000322",
		},
		{
			name: "an embedding has no reply to price", family: openai.FamilyEmbeddings, budget: 1,
			body:     `{"model":"team-chat","input":"hello"}`,
			attempts: []runtime.Attempt{{Price: costPrice()}},
			want:     "0.000002",
		},
		{
			name: "failing over reserves the dearest attempt and not their sum", family: openai.FamilyChat, budget: 2,
			body: `{"model":"team-chat","max_tokens":16,"messages":` + prompt + `}`,
			attempts: []runtime.Attempt{
				{Price: &usage.RoutingPrice{Price: usage.Price{InputPerMillion: costText("0.5"), OutputPerMillion: costText("5")}}},
				{Price: costPrice()},
			},
			want: "0.000162",
		},
		{
			name: "an attempt past the budget cannot be dispatched", family: openai.FamilyChat, budget: 1,
			body: `{"model":"team-chat","max_tokens":16,"messages":` + prompt + `}`,
			attempts: []runtime.Attempt{
				{Price: &usage.RoutingPrice{Price: usage.Price{InputPerMillion: costText("0.5"), OutputPerMillion: costText("5")}}},
				{Price: costPrice()},
			},
			want: "0.000081",
		},
		{
			name: "an attempt nobody can price reserves nothing of its own", family: openai.FamilyChat, budget: 2,
			body:     `{"model":"team-chat","max_tokens":16,"messages":` + prompt + `}`,
			attempts: []runtime.Attempt{{}, {Price: costPrice()}},
			want:     "0.000162",
		},
		{
			name: "a price without an output rate prices no generation", family: openai.FamilyChat, budget: 1,
			body:     `{"model":"team-chat","max_tokens":16,"messages":` + prompt + `}`,
			attempts: []runtime.Attempt{{Price: &usage.RoutingPrice{Price: usage.Price{InputPerMillion: costText("1")}}}},
			want:     "",
		},
		{
			name: "no attempt is priced", family: openai.FamilyChat, budget: 1,
			body:     `{"model":"team-chat","max_tokens":16,"messages":` + prompt + `}`,
			attempts: []runtime.Attempt{{}},
			want:     "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := costExecution(t, tc.body, tc.family, tc.budget, tc.attempts...)
			x.request.id, x.request.minted = uuid.NewString(), true
			hold := (&Server{}).costReservation(x, budgeted())
			if hold.amount != tc.want {
				t.Fatalf("reserves %q, want %q", hold.amount, tc.want)
			}
			if tc.want != "" && hold.requestID != x.request.id {
				t.Fatalf("reserved under %q, want the accounting identifier %q", hold.requestID, x.request.id)
			}
			if tc.want == "" && hold.requestID != "" {
				t.Fatalf("a request that reserves nothing names %q", hold.requestID)
			}
		})
	}
}

// TestKeysWithoutACostBudgetEstimateNoCost proves the keys that carry a cost
// budget are told apart from those that do not, and that a request with nothing to
// estimate holds nothing. That such a key is not even priced is proven by
// TestKeyWithoutCostBudgetEstimatesNothingEvenWhenPriced.
func TestKeysWithoutACostBudgetEstimateNoCost(t *testing.T) {
	rpm := int64(10)
	for _, authority := range []access.Authority{
		admissionAuthority(access.KeyPolicy{}),
		admissionAuthority(access.KeyPolicy{RequestsPerMinute: &rpm}),
		func() access.Authority {
			authority := admissionAuthority(access.KeyPolicy{})
			group := uuid.NewString()
			authority.BudgetGroupID = &group
			return authority
		}(),
	} {
		if hold := (&Server{}).costReservation(&execution{}, authority); hold != (costReservation{}) {
			t.Fatalf("estimated %+v for a key without a cost budget", hold)
		}
	}
	group := uuid.NewString()
	monthly := "50"
	inGroup := admissionAuthority(access.KeyPolicy{})
	inGroup.BudgetGroupID, inGroup.BudgetGroupMonthlyCostLimit = &group, &monthly
	if !costBudgeted(inGroup) || !costBudgeted(budgeted()) {
		t.Fatal("a cost budget on the key or its group was not noticed")
	}
}

// TestKeyWithoutCostBudgetEstimatesNothingEvenWhenPriced proves a feature nobody
// configured costs a request nothing: the request here has a price list to be
// priced by, and a key with no cost budget still has no estimate made for it. The
// guard is what spares every request of such a key the tokenizer and the price
// arithmetic, and nothing else observable tells it from an estimate that was made
// and thrown away.
func TestKeyWithoutCostBudgetEstimatesNothingEvenWhenPriced(t *testing.T) {
	x := costExecution(t, `{"model":"team-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, openai.FamilyChat, 1,
		runtime.Attempt{Price: costPrice()})
	x.request.id, x.request.minted = uuid.NewString(), true
	rpm := int64(10)
	group := uuid.NewString()
	inGroupWithoutBudget := admissionAuthority(access.KeyPolicy{})
	inGroupWithoutBudget.BudgetGroupID = &group
	for name, authority := range map[string]access.Authority{
		"no limits":              admissionAuthority(access.KeyPolicy{}),
		"a request limit":        admissionAuthority(access.KeyPolicy{RequestsPerMinute: &rpm}),
		"a group without budget": inGroupWithoutBudget,
	} {
		if hold := (&Server{}).costReservation(x, authority); hold != (costReservation{}) {
			t.Fatalf("%s: a key without a cost budget was estimated: %+v", name, hold)
		}
	}
	// The same request is priced the moment a budget applies, so the empty answers
	// above are the guard's and not an execution nothing could price.
	if hold := (&Server{}).costReservation(x, budgeted()); hold.amount != "0.000162" {
		t.Fatalf("a budgeted key reserves %q, want 0.000162", hold.amount)
	}
}

// TestEmbeddingPricedOnInputAloneIsReserved proves an embedding is priced on its
// input: it has no reply, and the price list of an embedding model carries no
// output rate, so requiring one would leave every embeddings request unreserved.
func TestEmbeddingPricedOnInputAloneIsReserved(t *testing.T) {
	x := costExecution(t, `{"model":"team-chat","input":"hello"}`, openai.FamilyEmbeddings, 1,
		runtime.Attempt{Price: &usage.RoutingPrice{Price: usage.Price{InputPerMillion: costText("1")}}})
	x.request.id, x.request.minted = uuid.NewString(), true
	if hold := (&Server{}).costReservation(x, budgeted()); hold.amount != "0.000002" {
		t.Fatalf("an embedding priced on input alone reserves %q, want 0.000002", hold.amount)
	}
	// A generation priced the same way has an output nobody can price.
	chat := costExecution(t, `{"model":"team-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, openai.FamilyChat, 1,
		runtime.Attempt{Price: &usage.RoutingPrice{Price: usage.Price{InputPerMillion: costText("1")}}})
	chat.request.id, chat.request.minted = uuid.NewString(), true
	if hold := (&Server{}).costReservation(chat, budgeted()); hold.amount != "" {
		t.Fatalf("a generation without an output rate reserves %q, want nothing", hold.amount)
	}
}

// TestAttemptCostBoundReservesNothingItCannotDescribe covers the attempts that
// have no bound: none whose provider the snapshot does not hold, and none whose
// request cannot be prepared for the provider. Either is left to be accounted as
// unpriced rather than reserved for at a guess.
func TestAttemptCostBoundReservesNothingItCannotDescribe(t *testing.T) {
	const body = `{"model":"team-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`
	x := costExecution(t, body, openai.FamilyChat, 1, runtime.Attempt{Price: costPrice()})
	if bound := x.attemptCostBound(x.attempts[0]); bound.String() != "0.000162" {
		t.Fatalf("the base attempt bounds %q, want 0.000162", bound.String())
	}
	gone := x.attempts[0]
	gone.ProviderID = uuid.NewString()
	if bound := x.attemptCostBound(gone); !bound.IsZero() {
		t.Fatalf("an attempt on a provider the snapshot does not hold bounds %q", bound.String())
	}
	// A strict route sends a target what the caller sent, which needs a compiled
	// interaction for the target; this route has none.
	strict := costExecution(t, body, openai.FamilyChat, 1, runtime.Attempt{Price: costPrice()})
	strict.route.Fidelity = runtime.RouteFidelity{Mode: runtime.FidelityStrict}
	if bound := strict.attemptCostBound(strict.attempts[0]); !bound.IsZero() {
		t.Fatalf("an attempt whose request cannot be prepared bounds %q", bound.String())
	}
}

func TestSettledCostPricesWhatEachAttemptReported(t *testing.T) {
	price := &usage.RoutingPrice{Price: usage.Price{
		InputPerMillion: costText("2"), OutputPerMillion: costText("8"), CachedInputPerMillion: costText("0.5"),
	}}
	observed := func(input, output int64) AttemptFact {
		return AttemptFact{
			Price: price, UsageObserved: true, UsageComplete: true,
			Usage: &openai.Usage{InputTokens: input, OutputTokens: output},
		}
	}
	cached := observed(1_000_000, 0)
	cached.Usage.CachedInputTokens = costCount(500_000)
	for _, tc := range []struct {
		name      string
		operation string
		facts     []AttemptFact
		want      string
	}{
		{"no attempt is no cost", "generation", nil, "0"},
		{"input and output", "generation", []AttemptFact{observed(1_000_000, 500_000)}, "6"},
		{"a failover bills every attempt that reported", "generation", []AttemptFact{observed(1_000_000, 0), observed(0, 1_000_000)}, "10"},
		{"cached input bills at its own rate", "generation", []AttemptFact{cached}, "1.25"},
		{"an embedding is priced on input alone", "embeddings", []AttemptFact{observed(1_000_000, 777)}, "2"},
		{"usage nobody observed is not billed here", "generation", []AttemptFact{
			{Price: price, UsageComplete: true},
			{Price: price, BillingUncertain: true},
			{Price: price, ResponseUsageDeferred: true, UsageComplete: true},
		}, "0"},
		{"an attempt without a price list is not priced here", "generation", []AttemptFact{
			{UsageObserved: true, UsageComplete: true, Usage: &openai.Usage{InputTokens: 5}},
		}, "0"},
		{"usage that is not complete is not priced", "generation", []AttemptFact{
			{Price: price, UsageObserved: true, Usage: &openai.Usage{InputTokens: 1_000_000}},
		}, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := &execution{facts: tc.facts, family: openai.FamilyChat}
			if tc.operation == "embeddings" {
				x.family = openai.FamilyEmbeddings
			}
			if got := x.settledCost().String(); got != tc.want {
				t.Fatalf("settledCost = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSettleAdmissionNeedsNoLeaseToFinish covers the paths that hold no
// reservation: a request without a lease settles silently, and a lease that
// reserved nothing is never told a cost.
func TestSettleAdmissionNeedsNoLeaseToFinish(t *testing.T) {
	s := &Server{log: slog.New(slog.DiscardHandler)}
	s.settleAdmission(context.Background(), &execution{dispatched: true})
	s.settleAdmission(context.Background(), &execution{})
}
