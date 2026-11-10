package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/usage"
)

func costText(value string) *string { return &value }

func costCount(value int64) *int64 { return &value }

func costPrice() *usage.RoutingPrice {
	return &usage.RoutingPrice{Price: usage.Price{
		InputPerMillion: costText("1"), OutputPerMillion: costText("10"),
	}}
}

func TestWorkflowCostRejectsPartiallyDecodedModelFacts(t *testing.T) {
	for _, tc := range []struct {
		name, metadata, want string
	}{
		{"bounded", `{"context_length":100,"max_output_tokens":10}`, ""},
		{"malformed parameter declaration", `{"context_length":100,"max_output_tokens":10,"supported_parameters":42}`, "run_cost_unbounded"},
		{"missing", "", "run_cost_unbounded"},
		{"null", "null", "run_cost_unbounded"},
		{"over budget", `{"context_length":1000000,"max_output_tokens":1000000}`, "run_cost_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.NewString()
			snapshot := &routing.Snapshot{Generation: routing.Generation{ID: uuid.NewString()}, Providers: map[string]routing.Provider{
				id: {ID: id, Name: "provider", Kind: "openai", RevisionID: id, Models: map[string]json.RawMessage{"model": json.RawMessage(tc.metadata)}},
			}}
			for _, compile := range []bool{false, true} {
				if compile {
					if err := snapshot.Compile(); err != nil {
						t.Fatal(err)
					}
				}
				x := &call{family: wire.Chat, historicalSnapshot: snapshot.ReadOnly(), workflow: &workflowCall{}, unary: &unaryCall{}, generation: generationCall{runLimits: limits.WorkflowBounds{MaxRunCost: new("1")}}}
				err := x.workflowModelCost(routing.Attempt{ProviderID: id, UpstreamModel: "model", Price: costPrice()})
				if tc.want == "" {
					if err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err == nil || err.Code != tc.want {
					t.Fatalf("workflow error %v, want %s", err, tc.want)
				}
			}
		})
	}
}

func costExecution(t *testing.T, body string, family wire.Format, budget int, attempts ...routing.Attempt) *call {
	t.Helper()
	request, err := protocols.Parse(family, []byte(body), "team-chat")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &routing.Snapshot{Providers: map[string]routing.Provider{}}
	for index := range attempts {
		id := uuid.NewString()
		attempts[index].ProviderID, attempts[index].TargetID = id, uuid.NewString()
		if attempts[index].UpstreamModel == "" {
			attempts[index].UpstreamModel = "model-a"
		}
		snapshot.Providers[id] = routing.Provider{
			ID: id, AuthMode: "none", Slots: []routing.Slot{{ID: uuid.NewString(), Enabled: true}},
		}
	}
	return &call{
		family:             family,
		historicalSnapshot: snapshot.ReadOnly(), routeLeg: routeLeg{route: &routing.Route{Slug: "team-chat", Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed}},
			attempts: attempts, budget: budget}, generation: generationCall{parsed: request},
	}
}

func budgeted() keyauthority.Authority {
	return admissionAuthority(keyauthority.KeyPolicy{DailyCostLimit: costText("5.00")})
}

func TestAutomaticReservationsPriceEffectiveGeminiOutputControls(t *testing.T) {
	const chat = `"model":"team-chat","messages":[{"role":"user","content":"hello"}]`
	const gemini = `"contents":[{"role":"user","parts":[{"text":"hello"}]}]`
	for _, tc := range []struct {
		name       string
		family     wire.Format
		body       string
		output     int64
		candidates int64
		reply      int64
		cost       string
	}{
		{"translated defaults", wire.Chat, `{` + chat + `}`, 32768, 4, 131072, "1.310722"},
		{"translated sampling", wire.Chat, `{` + chat + `,"temperature":0.2}`, 32768, 4, 131072, "1.310722"},
		{"native sampling", wire.Gemini, `{` + gemini + `,"generationConfig":{"temperature":0.2}}`, 32768, 4, 131072, "1.310722"},
		{"translated output override", wire.Chat, `{` + chat + `,"max_tokens":16}`, 16, 4, 64, "0.000642"},
		{"translated candidate override", wire.Chat, `{` + chat + `,"n":2}`, 32768, 2, 65536, "0.655362"},
		{"translated both overrides", wire.Chat, `{` + chat + `,"max_tokens":16,"n":2}`, 16, 2, 32, "0.000322"},
		{"native output override", wire.Gemini, `{` + gemini + `,"generationConfig":{"maxOutputTokens":16}}`, 16, 4, 64, "0.000642"},
		{"native candidate override", wire.Gemini, `{` + gemini + `,"generationConfig":{"candidateCount":2}}`, 32768, 2, 65536, "0.655362"},
		{"native null output", wire.Gemini, `{` + gemini + `,"generationConfig":{"maxOutputTokens":null}}`, 0, 4, 16384, "0.163842"},
		{"native null config", wire.Gemini, `{` + gemini + `,"generationConfig":null}`, 0, 0, 4096, "0.040962"},
	} {
		for _, preparation := range []string{"unplanned", "planned", "body not kept"} {
			t.Run(tc.name+"/"+preparation, func(t *testing.T) {
				x := costExecution(t, tc.body, tc.family, 1, routing.Attempt{UpstreamModel: "gemini-2.5-flash", Price: costPrice()})
				x.request.id, x.request.minted = uuid.NewString(), true
				attempt := x.attempts[0]
				snapshot := x.snapshot().Clone()
				provider := snapshot.Providers[attempt.ProviderID]
				provider.Kind = "gemini"
				provider.ParameterDefaults = protocols.Object{"generationConfig": json.RawMessage(`{"maxOutputTokens":32768,"candidateCount":4}`)}
				snapshot.Providers[provider.ID] = provider
				x.historicalSnapshot = snapshot.ReadOnly()
				if preparation == "body not kept" {
					for range keptEncodings {
						other := provider
						other.ID = uuid.NewString()
						if err := x.encodes(&other, other.Connector(), attempt.TargetID, attempt.UpstreamModel); err != nil {
							t.Fatal(err)
						}
					}
				}
				if preparation != "unplanned" {
					if err := x.encodes(&provider, provider.Connector(), attempt.TargetID, attempt.UpstreamModel); err != nil {
						t.Fatal(err)
					}
				}
				if hold := (&Server{}).costReservation(x, budgeted()); hold.amount != tc.cost {
					t.Fatalf("cost reservation = %q, want %q", hold.amount, tc.cost)
				}
				body, encodedWire, _, err := x.encoding(&provider, provider.Connector(), attempt.TargetID, attempt.UpstreamModel)
				if err != nil || encodedWire != wire.Gemini {
					t.Fatalf("encoding wire = %s, error = %v", encodedWire, err)
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
				admitted, err := x.attemptEstimate(&provider, attempt.TargetID, attempt.UpstreamModel)
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

// Fallback dispatch still spends against the ingress policy.

// The same request is priced the moment a budget applies, so the empty answers
// above are the guard's and not an execution nothing could price.

// A generation priced the same way has an output nobody can price.

// A strict route sends a target what the caller sent, which needs a compiled
// interaction for the target; this route has none.

func TestSettledCostPricesWhatEachAttemptReported(t *testing.T) {
	price := &usage.RoutingPrice{Price: usage.Price{
		InputPerMillion: costText("2"), OutputPerMillion: costText("8"), CachedInputPerMillion: costText("0.5"),
	}}
	observed := func(input, output int64) AttemptFact {
		return AttemptFact{
			Price: price, UsageObserved: true, UsageComplete: true,
			Usage: &wire.Usage{InputTokens: input, OutputTokens: output},
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
			{Price: price, GenerationUsageDeferred: true, UsageComplete: true},
		}, "0"},
		{"an attempt without a price list is not priced here", "generation", []AttemptFact{
			{UsageObserved: true, UsageComplete: true, Usage: &wire.Usage{InputTokens: 5}},
		}, "0"},
		{"usage that is not complete is not priced", "generation", []AttemptFact{
			{Price: price, UsageObserved: true, Usage: &wire.Usage{InputTokens: 1_000_000}},
		}, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := &call{facts: tc.facts, family: wire.Chat}
			if tc.operation == "embeddings" {
				x.family = wire.Embeddings
			}
			if got := x.settledCost().String(); got != tc.want {
				t.Fatalf("settledCost = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRouteChargeBoundSortsDearestRetriesAndVideoCard(t *testing.T) {
	cheap := routing.Attempt{UpstreamModel: "cheap", Price: &usage.RoutingPrice{Price: usage.Price{
		Model: "cheap", Operation: "generation",
		InputPerMillion: costText("1"), OutputPerMillion: costText("1"),
	}}}
	dear := routing.Attempt{UpstreamModel: "dear", Price: &usage.RoutingPrice{Price: usage.Price{
		Model: "dear", Operation: "generation",
		InputPerMillion: costText("1"), OutputPerMillion: costText("1"),
	}}}
	x := costExecution(t, `{"model":"team-chat","max_tokens":1000,"messages":[{"role":"user","content":"hello"}]}`, wire.Chat, 2, cheap, dear)
	x.route.Retry = routing.Retry{classRateLimit: {MaxRetries: 1}}
	x.cardPinned = true
	x.cardRev = &usage.RoutingCardRevision{Markup: "1", Prices: []usage.RoutingPrice{{Price: usage.Price{
		Model: "dear", Operation: "generation",
		InputPerMillion: costText("100"), OutputPerMillion: costText("100"),
	}}}}
	s := &Server{}
	got := s.routeChargeBound(x)
	want := x.attemptChargeBound(x.attempts[1], x.attemptCostBound(x.attempts[1]))
	want = want.Add(want)
	if got.String() != want.String() {
		t.Fatalf("routeChargeBound = %s, want %s (two dispatches of dearest explicit rate)", got, want)
	}

	video, failure := media.DecodeVideoCreate(staging.FormOf([]staging.Field{
		{Name: "model", Value: staging.Text("team-chat")},
		{Name: "prompt", Value: staging.Text("A cat.")},
		{Name: "seconds", Value: staging.Text("4")},
	}, false))
	if failure != nil {
		t.Fatal(failure.Message)
	}
	videoAttempt := routing.Attempt{ProviderID: x.attempts[0].ProviderID, UpstreamModel: "sora-2",
		Price: &usage.RoutingPrice{Price: usage.Price{Model: "sora-2", Operation: "video_create", UnitPrice: costText("0.25")}}}
	videoExec := &call{
		family:             wire.Format("video_create"),
		media:              video,
		request:            request{id: "video-charge", minted: true},
		historicalSnapshot: x.historicalSnapshot,
		cardPinned:         true,
		cardRev:            &usage.RoutingCardRevision{Markup: "1.5"},
	}
	if hold := videoExec.attemptCostReservation(budgeted(), videoAttempt); hold.amount != "1" || hold.charge != "1.5" {
		t.Fatalf("video attemptCostReservation = %+v, want amount=1 charge=1.5", hold)
	}
}

func TestSettledChargeRetainsEachAttemptCardRevision(t *testing.T) {
	for _, owner := range []string{"provider", "route", "installation", "authority"} {
		t.Run(owner, func(t *testing.T) {
			now := time.Now().UTC()
			x := costExecution(t, `{"model":"team-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, wire.Chat, 3,
				routing.Attempt{Price: costPrice()})
			x.request.startedAt = now
			project := uuid.NewString()
			authority := keyauthority.Authority{}
			card := usage.RoutingRateCard{ProjectID: &project, Revisions: []usage.RoutingCardRevision{{
				ID: uuid.NewString(), Markup: "2", EffectiveAt: now.Add(-time.Hour),
			}}}
			snapshot := x.snapshot().Clone()
			provider := snapshot.Providers[x.attempts[0].ProviderID]
			switch owner {
			case "provider":
				provider.ProjectID = &project
				snapshot.Providers[provider.ID] = provider
				x.historicalSnapshot = snapshot.ReadOnly()
			case "route":
				x.route.ProjectID = &project
			case "installation":
				card.ProjectID = nil
			case "authority":
				authority.ProjectID = &project
			}
			rt := &fakeRuntime{inputs: &usage.RoutingInputs{RefreshedAt: now, RateCards: []usage.RoutingRateCard{card}}}
			s := &Server{now: func() time.Time { return now }, fleet: &fleetHealth{}, deps: Dependencies{Runtime: rt}}
			s.bindAuthority(x, authority)
			dispatch := func() {
				fact := s.newFact(x, x.attempts[0], provider.Slots[0], len(x.facts)+1)
				fact.Class, fact.UsageObserved, fact.UsageComplete = classSuccess, true, true
				fact.Usage = &wire.Usage{InputTokens: 100, OutputTokens: 50}
				x.facts = append(x.facts, fact)
			}
			dispatch()

			// A retry outlives the original inputs while the runtime refreshes.
			now = now.Add(usage.PerformanceMaxAge + time.Second)
			rt.inputs = &usage.RoutingInputs{RefreshedAt: now, RateCards: []usage.RoutingRateCard{card}}
			dispatch()

			// Replanning sees only the new in-force revision. Earlier facts
			// must retain their original terms even if their first settlement
			// happens after that revision is pruned.
			now = now.Add(usage.PerformanceMaxAge + time.Second)
			card.Revisions = []usage.RoutingCardRevision{{ID: uuid.NewString(), Markup: "3", EffectiveAt: now}}
			rt.inputs = &usage.RoutingInputs{RefreshedAt: now, RateCards: []usage.RoutingRateCard{card}}
			s.selectionOptions(context.Background(), x)
			if got := s.settledCharge(x).String(); got != "0.0024" {
				t.Fatalf("charge after replan = %s, want 0.0024", got)
			}
			dispatch()
			want := "0.0042"
			if owner == "authority" {
				want = "0.0036" // The authority's admission pin covers every attempt.
			}
			if got := s.settledCharge(x).String(); got != want {
				t.Fatalf("charge with new revision = %s, want %s", got, want)
			}
		})
	}
}

func TestRetainedPageOnlyAdmitsCostsForEntriesBeingRead(t *testing.T) {
	now := time.Now()
	provider := routing.Provider{ID: uuid.NewString(), Kind: "openai"}
	x := &call{
		actor: "api_key_resource", operationOverride: "fine_tuning",
		retained:           retainedOperation{use: retainedHousekeeping, usage: retainedTokenlessUsage},
		request:            request{startedAt: now},
		historicalSnapshot: (&routing.Snapshot{Providers: map[string]routing.Provider{provider.ID: provider}}).ReadOnly(),
		routingInputs: &usage.RoutingInputs{RefreshedAt: now, Prices: []usage.RoutingPrice{
			{Price: usage.Price{ProviderKind: provider.Kind, Model: "free", Operation: "fine_tuning", PerRequest: new("0")}},
			{Price: usage.Price{ProviderKind: provider.Kind, Model: "paid", Operation: "fine_tuning", PerRequest: new("1")}},
		}},
	}
	s := &Server{now: func() time.Time { return now }, deps: Dependencies{Admission: &Admission{
		CostAccountingReady: func(context.Context) error { return errors.New("accounting unavailable") },
	}}}
	prepare := func(model string) retainedListEntry {
		t.Helper()
		pin := &pin{provider: provider, model: model, attempt: routing.Attempt{ProviderID: provider.ID, UpstreamModel: model}}
		admission, err := s.prepareRetainedAdmission(x, pin, false)
		if err != nil {
			t.Fatal(err)
		}
		return retainedListEntry{pin: pin, admission: admission}
	}
	free, paid := prepare("free"), prepare("paid")
	for _, tc := range []struct {
		name    string
		entries []retainedListEntry
		limit   int
		blocked bool
	}{
		{"free page with paid continuation", []retainedListEntry{free, paid}, 1, false},
		{"mixed page", []retainedListEntry{free, paid}, 2, true},
		{"paid page with free continuation", []retainedListEntry{paid, free}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := s.reserveRetainedPage(context.Background(), x, budgeted(), tc.entries, tc.limit, time.Second)
			if tc.blocked && (failure == nil || failure.Code != "cost_accounting_incomplete") {
				t.Fatalf("paid page admission = %v", failure)
			}
			if !tc.blocked && failure != nil {
				t.Fatalf("free page admission = %v", failure)
			}
		})
	}
	if failure := s.admitRetainedPageCost(context.Background(), x, budgeted(), time.Second); failure == nil || failure.Code != "cost_accounting_incomplete" {
		t.Fatalf("paid refill admission = %v", failure)
	}
}
