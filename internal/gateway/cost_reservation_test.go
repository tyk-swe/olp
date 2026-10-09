package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/protocols"
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
	request, err := protocols.Parse(family, []byte(body), "team-chat")
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

func TestVideoCostBoundUsesRequestedDurationAndProviderDefault(t *testing.T) {
	for _, tc := range []struct {
		seconds *string
		want    string
	}{{nil, "0.4"}, {costText("8"), "0.8"}, {costText("12"), "1.2"}} {
		x := &execution{media: &media.Request{Op: media.OpVideoCreate, Seconds: tc.seconds}}
		attempt := runtime.Attempt{Price: &usage.RoutingPrice{Price: usage.Price{UnitPrice: costText("0.1")}}}
		if got := x.attemptCostBound(attempt).String(); got != tc.want {
			t.Fatalf("duration %v reserves %s, want %s", tc.seconds, got, tc.want)
		}
	}
}

func TestUnitPricedVideoUsageSettlesWithoutTokenRates(t *testing.T) {
	owner := uuid.NewString()
	x := &execution{media: &media.Request{Op: media.OpVideoCreate}, facts: []AttemptFact{{
		Budgets: []string{owner}, UsageObserved: true, UsageComplete: true,
		Usage: &openai.Usage{MediaUnits: costText("8")}, Price: &usage.RoutingPrice{Price: usage.Price{UnitPrice: costText("0.1")}},
	}}}
	if got := x.costOf(owner).String(); got != "0.8" {
		t.Fatalf("unit-only video settled %s, want 0.8", got)
	}
	input, output, _ := accountingTokens(&x.facts[0])
	if input != nil || output != nil {
		t.Fatal("unit-only media accounting invented token usage")
	}
}

func TestCostReservationIsTheMostTheRequestCouldCost(t *testing.T) {
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
			// Settlement bills every attempt that reports usage, so a failover
			// adds each attempt's bound rather than taking the dearest.
			name: "failing over reserves the sum of the attempts", family: openai.FamilyChat, budget: 2,
			body: `{"model":"team-chat","max_tokens":16,"messages":` + prompt + `}`,
			attempts: []runtime.Attempt{
				{Price: &usage.RoutingPrice{Price: usage.Price{InputPerMillion: costText("0.5"), OutputPerMillion: costText("5")}}},
				{Price: costPrice()},
			},
			want: "0.000243",
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

// TestCredentialSlotRetriesAreEachReserved proves a request that can be
// retried through another credential slot reserves every dispatch it could
// bill: a first attempt billed without settling does not refund what a retry
// will cost, so the bound is the sum over the slots the walk visits.
func TestCredentialSlotRetriesAreEachReserved(t *testing.T) {
	x := costExecution(t, `{"model":"team-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, openai.FamilyChat, 2,
		runtime.Attempt{Price: costPrice()})
	x.request.id, x.request.minted = uuid.NewString(), true
	provider := x.snapshot().Providers[x.attempts[0].ProviderID]
	provider.Slots = append(provider.Slots, runtime.Slot{ID: uuid.NewString(), Enabled: true})
	x.snapshot().Providers[provider.ID] = provider
	if hold := (&Server{}).costReservation(x, budgeted()); hold.amount != "0.000324" {
		t.Fatalf("two billable dispatches reserve %q, want 0.000324", hold.amount)
	}
	// A request whose attempt budget is one still reserves a single dispatch.
	x.budget = 1
	if hold := (&Server{}).costReservation(x, budgeted()); hold.amount != "0.000162" {
		t.Fatalf("one dispatch reserves %q, want 0.000162", hold.amount)
	}
}

func TestSameSlotRetriesReserveTokensAndCostWithinTheAttemptBudget(t *testing.T) {
	x := costExecution(t, `{"model":"team-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, openai.FamilyChat, 3,
		runtime.Attempt{Price: costPrice()})
	x.request.id, x.request.minted = uuid.NewString(), true
	x.route.Retry = runtime.Retry{
		classRateLimit:      {MaxRetries: 2},
		classUpstreamServer: {MaxRetries: 1},
	}
	s := &Server{}
	for _, tc := range []struct {
		budget int
		cost   string
	}{{3, "0.000486"}, {2, "0.000324"}, {1, "0.000162"}, {0, ""}} {
		x.budget = tc.budget
		if got := s.dispatchableAttempts(x); got != tc.budget {
			t.Fatalf("budget %d reserves %d dispatches", tc.budget, got)
		}
		if got := keyReservationEstimate(18, s.dispatchableAttempts(x)); got != 18*int64(max(tc.budget, 1)) {
			t.Fatalf("budget %d reserves %d tokens", tc.budget, got)
		}
		if hold := s.costReservation(x, budgeted()); hold.amount != tc.cost {
			t.Fatalf("budget %d reserves %q, want %q", tc.budget, hold.amount, tc.cost)
		}
	}
}

func TestRetryCostReservationCoversSkippingCheapRetries(t *testing.T) {
	x := costExecution(t, `{"model":"team-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, openai.FamilyChat, 3,
		runtime.Attempt{Price: &usage.RoutingPrice{Price: usage.Price{InputPerMillion: costText("0.1"), OutputPerMillion: costText("1")}}},
		runtime.Attempt{Price: costPrice()})
	x.request.id, x.request.minted = uuid.NewString(), true
	x.route.Retry = runtime.Retry{classRateLimit: {MaxRetries: 2}}
	if hold := (&Server{}).costReservation(x, budgeted()); hold.amount != "0.000486" {
		t.Fatalf("reserves %q, want enough for three expensive dispatches", hold.amount)
	}
}

func TestAutomaticReservationsPriceEffectiveGeminiOutputControls(t *testing.T) {
	const chat = `"model":"team-chat","messages":[{"role":"user","content":"hello"}]`
	const gemini = `"contents":[{"role":"user","parts":[{"text":"hello"}]}]`
	for _, tc := range []struct {
		name       string
		family     openai.Family
		body       string
		output     int64
		candidates int64
		reply      int64
		cost       string
	}{
		{"translated defaults", openai.FamilyChat, `{` + chat + `}`, 32768, 4, 131072, "1.310722"},
		{"translated sampling", openai.FamilyChat, `{` + chat + `,"temperature":0.2}`, 32768, 4, 131072, "1.310722"},
		{"native sampling", openai.FamilyGemini, `{` + gemini + `,"generationConfig":{"temperature":0.2}}`, 32768, 4, 131072, "1.310722"},
		{"translated output override", openai.FamilyChat, `{` + chat + `,"max_tokens":16}`, 16, 4, 64, "0.000642"},
		{"translated candidate override", openai.FamilyChat, `{` + chat + `,"n":2}`, 32768, 2, 65536, "0.655362"},
		{"translated both overrides", openai.FamilyChat, `{` + chat + `,"max_tokens":16,"n":2}`, 16, 2, 32, "0.000322"},
		{"native output override", openai.FamilyGemini, `{` + gemini + `,"generationConfig":{"maxOutputTokens":16}}`, 16, 4, 64, "0.000642"},
		{"native candidate override", openai.FamilyGemini, `{` + gemini + `,"generationConfig":{"candidateCount":2}}`, 32768, 2, 65536, "0.655362"},
		{"native null output", openai.FamilyGemini, `{` + gemini + `,"generationConfig":{"maxOutputTokens":null}}`, 0, 4, 16384, "0.163842"},
		{"native null config", openai.FamilyGemini, `{` + gemini + `,"generationConfig":null}`, 0, 0, 4096, "0.040962"},
	} {
		for _, preparation := range []string{"unplanned", "planned", "body not kept"} {
			t.Run(tc.name+"/"+preparation, func(t *testing.T) {
				x := costExecution(t, tc.body, tc.family, 1, runtime.Attempt{UpstreamModel: "gemini-2.5-flash", Price: costPrice()})
				x.request.id, x.request.minted = uuid.NewString(), true
				attempt := x.attempts[0]
				provider := x.snapshot().Providers[attempt.ProviderID]
				provider.Kind = "gemini"
				provider.ParameterDefaults = protocols.Object{"generationConfig": json.RawMessage(`{"maxOutputTokens":32768,"candidateCount":4}`)}
				x.snapshot().Providers[provider.ID] = provider
				if preparation == "body not kept" {
					for range keptEncodings {
						other := provider
						other.ID = uuid.NewString()
						if err := x.encodes(&other, other.Connector(), attempt.UpstreamModel); err != nil {
							t.Fatal(err)
						}
					}
				}
				if preparation != "unplanned" {
					if err := x.encodes(&provider, provider.Connector(), attempt.UpstreamModel); err != nil {
						t.Fatal(err)
					}
				}
				if hold := (&Server{}).costReservation(x, budgeted()); hold.amount != tc.cost {
					t.Fatalf("cost reservation = %q, want %q", hold.amount, tc.cost)
				}
				body, wire, err := x.encoding(x.takeEncoded(), &provider, provider.Connector(), attempt.UpstreamModel)
				if err != nil || wire != openai.FamilyGemini {
					t.Fatalf("encoding wire = %s, error = %v", wire, err)
				}
				var encoded struct {
					Config struct {
						Output     int64 `json:"maxOutputTokens"`
						Candidates int64 `json:"candidateCount"`
					} `json:"generationConfig"`
				}
				if err := json.Unmarshal(body, &encoded); err != nil {
					t.Fatal(err)
				}
				if encoded.Config.Output != tc.output || encoded.Config.Candidates != tc.candidates {
					t.Fatalf("encoded controls = %+v, want output %d and candidates %d", encoded.Config, tc.output, tc.candidates)
				}
				// The bounds must remain usable after dispatch takes the kept bodies.
				admitted, err := x.attemptEstimate(&provider, attempt.UpstreamModel)
				if err != nil {
					t.Fatal(err)
				}
				if admitted.reply != tc.reply || admitted.reserve != 2+tc.reply {
					t.Fatalf("admitted = %+v, want reply %d and reservation %d", admitted, tc.reply, 2+tc.reply)
				}
			})
		}
	}
}

// TestKeysWithoutACostBudgetEstimateNoCost proves the keys that carry a cost
// budget are told apart from those that do not, and that a request with nothing to
// estimate holds nothing. That such a key is not even priced is proven by
// TestKeyWithoutCostBudgetEstimatesNothingEvenWhenPriced.
func TestVideoCreateHoldsItsTargetsPriceAgainstACostBudget(t *testing.T) {
	attempt := runtime.Attempt{Price: &usage.RoutingPrice{Price: usage.Price{UnitPrice: costText("0.1")}}}
	x := &execution{media: &media.Request{Op: media.OpVideoCreate, Seconds: costText("8")}, request: request{id: "video-create", minted: true}}
	if hold := x.attemptCostReservation(budgeted(), attempt); hold != (costReservation{amount: "0.8", requestID: "video-create"}) {
		t.Fatalf("a budgeted video create holds %+v", hold)
	}
	if hold := x.attemptCostReservation(admissionAuthority(access.KeyPolicy{}), attempt); hold != (costReservation{}) {
		t.Fatalf("a video create without a cost budget holds %+v", hold)
	}
}

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
	if !(&execution{}).costBudgeted(inGroup) || !(&execution{}).costBudgeted(budgeted()) {
		t.Fatal("a cost budget on the key or its group was not noticed")
	}
}

func TestCostAccountingGrowthUsesOnlyTheNamedRoutesBudget(t *testing.T) {
	for _, phase := range []string{"fallback", "session"} {
		for _, budgetRoute := range []string{"other", "team-chat"} {
			t.Run(phase+"/"+budgetRoute, func(t *testing.T) {
				x := costExecution(t, `{"model":"team-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, openai.FamilyChat, 1,
					runtime.Attempt{Price: costPrice()})
				x.request.id, x.request.minted = uuid.NewString(), true
				x.authority = admissionAuthority(access.KeyPolicy{RequestsPerMinute: costCount(100), TokensPerMinute: costCount(1000)})
				client := newAllowing(100, 1000, 18, false)
				admission := newAdmission(t, client)
				var refusal *Error
				x.lease, refusal = admission.reserveKey(t.Context(), x.authority, "openai", 18, time.Minute, "team-chat")
				if refusal != nil || x.lease == nil || x.lease.HasCostReservation() {
					t.Fatalf("rate lease: %v %v", x.lease, refusal)
				}
				x.authority.Policy.RouteLimits = access.RouteLimits{budgetRoute: {WeeklyCostLimit: costText("1")}}
				// Fallback dispatch still spends against the ingress policy.
				x.primary = x.route
				fallback := *x.route
				fallback.Slug = "fallback"
				x.route = &fallback
				checked := 0
				admission.CostAccountingReady = func(context.Context) error {
					checked++
					return errors.New("lost accounting")
				}
				s := &Server{Admission: admission}
				applicable := budgetRoute == "team-chat"
				for _, hold := range []costReservation{s.costReservation(x, x.authority), x.attemptCostReservation(x.authority, x.attempts[0])} {
					if (hold.amount != "") != applicable {
						t.Fatalf("cost estimate %+v does not match the named route budget", hold)
					}
				}
				before := client.calls.Load()
				if phase == "fallback" {
					refusal = s.reserveFallbackCost(t.Context(), x)
				} else {
					refusal = s.reserveSessionCost(t.Context(), x, x.authority, x.attempts[0], time.Minute)
				}
				if applicable {
					if refusal == nil || refusal.Code != "cost_accounting_incomplete" || checked != 1 {
						t.Fatalf("applicable budget bypassed accounting loss: %v checks=%d", refusal, checked)
					}
				} else if refusal != nil || checked != 0 {
					t.Fatalf("another route's budget quarantined this request: %v checks=%d", refusal, checked)
				}
				if client.calls.Load() != before {
					t.Fatal("growth reached the limiter after accounting refusal or without a cost budget")
				}
			})
		}
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
