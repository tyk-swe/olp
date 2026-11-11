package routing

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

func ptr[T any](v T) *T { return &v }

func planningFixture() (Snapshot, string, []string) {
	ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	s := Snapshot{Generation: Generation{ID: uuid.NewString()}, Providers: map[string]Provider{}, Routes: map[string]Route{}}
	routeID := uuid.NewString()
	route := Route{ID: routeID, Slug: "team-model", RoutingID: routeID, OverallTimeout: 3000, MaxAttempts: 3, Operations: []string{"generation"}, Fidelity: RouteFidelity{Mode: FidelityTransformed}}
	for i, id := range ids {
		target := uuid.NewString()
		s.Providers[id] = Provider{ID: id, Name: id, Kind: "openai", VendorID: "openai", Enabled: true, RevisionID: uuid.NewString(), Capabilities: []Capability{{Model: "wire-model", Operation: "generation", Surface: "openai", Mode: "unary"}}}
		route.Targets = append(route.Targets, Target{ID: target, RoutingID: target, ProviderID: id, ProviderModel: "wire-model", Weight: int64(i + 1), Timeout: 1000})
	}
	s.Routes[route.Slug] = route
	return s, route.Slug, ids
}
func TestPoliciesIntersectUnknownFactsAndPublishedLimits(t *testing.T) {
	s, slug, ids := planningFixture()
	now := time.Now()
	metadata, _ := json.Marshal(ModelMetadata{Region: ptr("eu"), ZeroDataRetention: ptr(true), DataCollection: ptr(false), Source: ptr("contract"), ObservedAt: &now, SupportedParameters: &[]string{"temperature"}})
	p := s.Providers[ids[0]]
	p.Models = map[string]json.RawMessage{"wire-model": metadata}
	s.Providers[p.ID] = p
	s.InstallationPolicy = &Policy{Constraints: Preferences{Regions: []string{"eu"}, RequireZeroDataRetention: ptr(true), RequireParameters: ptr(true)}}
	if err := s.Compile(); err != nil {
		t.Fatal(err)
	}
	plan, e := Select(&s, slug, "generation", "openai", "unary", []byte("seed"), Options{Parameters: Listed([]string{"temperature"}), Now: now})
	if e != nil || len(plan.Attempts) != 1 || plan.Attempts[0].ProviderID != ids[0] {
		t.Fatalf("facts: %+v %v", plan, e)
	}
	plan, e = Select(&s, slug, "generation", "openai", "unary", []byte("seed"), Options{Preferences: &Preferences{Only: []string{"provider:" + ids[1]}}, Now: now})
	if e != nil || len(plan.Attempts) != 0 {
		t.Fatalf("request expanded parent policy: %+v %v", plan, e)
	}
	if _, e = Select(&s, slug, "generation", "openai", "unary", []byte("seed"), Options{Preferences: &Preferences{MaxAttempts: ptr(4)}}); e == nil {
		t.Fatal("expanded route attempt budget")
	}
	s.InstallationPolicy = &Policy{AllowedStrategies: []string{"weighted"}}
	if _, e = Select(&s, slug, "generation", "openai", "unary", []byte("seed"), Options{Preferences: &Preferences{Strategy: ptr("price")}}); e == nil {
		t.Fatal("expanded strategy permission")
	}
	for _, payload := range []string{`{"deadline":9000}`, `{"only":["anything"]}`, `{"max_price":{"input_per_million":"1e9"}}`, `{"allowed_strategies":["price"]}`, `{"strategy":"price"} {}`, `null`} {
		if _, e = ParsePreferences([]byte(payload)); e == nil {
			t.Fatalf("accepted invalid controls %s", payload)
		}
	}
}

func TestModelFactsFollowSnapshotRecompilation(t *testing.T) {
	snapshot, slug, ids := planningFixture()
	provider := snapshot.Providers[ids[0]]
	provider.Models = map[string]json.RawMessage{"wire-model": json.RawMessage(`{"context_length":2048,"max_output_tokens":1024,"supported_parameters":["response_format"]}`)}
	snapshot.Providers[ids[0]] = provider
	options := Options{Now: time.Unix(1700000000, 0), Preferences: &Preferences{Only: []string{"provider:" + ids[0]}}, TokenDemand: &TokenDemand{EstimatedInputTokens: 512}}
	before, err := Select(&snapshot, slug, "generation", "openai", "unary", []byte("seed"), options)
	if err != nil || len(before.Attempts) != 1 {
		t.Fatalf("initial plan %+v, error %v", before, err)
	}
	digest, err := snapshot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	capabilityRoute := snapshot.Routes[slug]
	capabilityRoute.Targets = capabilityRoute.Targets[:1]
	capabilities := EffectiveCapabilities(&snapshot, capabilityRoute)
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	compiled, err := Select(&snapshot, slug, "generation", "openai", "unary", []byte("seed"), options)
	if err != nil || !reflect.DeepEqual(before, compiled) {
		t.Fatalf("compilation changed planning: %+v, error %v", compiled, err)
	}
	if got, err := snapshot.Digest(); err != nil || got != digest {
		t.Fatalf("compilation changed snapshot digest: %s, error %v", got, err)
	}
	if got := EffectiveCapabilities(&snapshot, capabilityRoute); !reflect.DeepEqual(got, capabilities) {
		t.Fatalf("compilation changed capabilities: %+v", got)
	}
	clone := snapshot.Clone()
	clone.Providers[ids[0]].Models["wire-model"] = json.RawMessage(`{"context_length":256,"supported_parameters":[]}`)
	for _, compile := range []bool{false, true} {
		if compile {
			if err := clone.Compile(); err != nil {
				t.Fatal(err)
			}
		}
		changed, err := Select(clone, slug, "generation", "openai", "unary", []byte("seed"), options)
		decision := slices.IndexFunc(changed.Decisions, func(d Decision) bool { return d.ProviderID == ids[0] })
		if err != nil || len(changed.Attempts) != 0 || decision < 0 || changed.Decisions[decision].Reason == nil || *changed.Decisions[decision].Reason != "context_length_exceeded" {
			t.Fatalf("new model limit was ignored: %+v, error %v", changed, err)
		}
		facts, valid := clone.Providers[ids[0]].Metadata("wire-model")
		if !valid || facts.SupportedParameters == nil || len(*facts.SupportedParameters) != 0 {
			t.Fatal("new model parameter declaration was ignored")
		}
	}
	clone.Providers[ids[0]].Models["wire-model"] = provider.Models["wire-model"]
	if err := clone.Compile(); err != nil {
		t.Fatal(err)
	}
	for _, current := range []*Snapshot{&snapshot, clone} {
		plan, err := Select(current, slug, "generation", "openai", "unary", []byte("seed"), options)
		if err != nil || !reflect.DeepEqual(plan, before) {
			t.Fatalf("model facts leaked between snapshots or remained stale: %+v, error %v", plan, err)
		}
	}
}

func TestPricePriorityFallbackAndPerformanceShareOneOrdering(t *testing.T) {
	s, slug, ids := planningFixture()
	now := time.Now()
	inputs := &routinginputs.Inputs{RefreshedAt: now, Performance: map[string]routinginputs.Performance{}}
	for i, id := range ids[:2] {
		amount := []string{"0.000000000002", "0.000000000001"}[i]
		inputs.Prices = append(inputs.Prices, *rates.NewRoutingPrice(rates.RoutingPrice{Price: rates.Price{ProviderID: &id, ProviderKind: "openai", Model: "wire-model", Operation: "generation", InputPerMillion: &amount, OutputPerMillion: &amount, Currency: "USD"}, RevisionID: uuid.NewString(), Revision: 1, EffectiveAt: now.Add(-time.Hour)}))
	}
	options := Options{Inputs: inputs, Now: now, Preferences: &Preferences{Strategy: ptr("price")}}
	plan, e := Select(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if e != nil || len(plan.Attempts) != 3 || plan.Attempts[0].ProviderID != ids[1] || plan.Attempts[2].ProviderID != ids[2] {
		t.Fatalf("exact prices: %+v %v", plan, e)
	}
	route := s.Routes[slug]
	route.Targets[0].Priority = -1
	s.Routes[slug] = route
	plan, e = Select(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if e != nil || plan.Attempts[0].ProviderID != ids[0] {
		t.Fatal("strategy escaped priority tier")
	}
	options.Preferences = &Preferences{Order: []string{"provider:" + ids[2]}, AllowFallbacks: ptr(false)}
	plan, e = Select(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if e != nil || len(plan.Attempts) != 1 || plan.Attempts[0].ProviderID != ids[2] {
		t.Fatal("explicit fallback boundary ignored")
	}
	route.Targets[0].Priority = 0
	s.Routes[slug] = route
	inputs.Performance[routinginputs.PerformanceKey(ids[1], "wire-model", "generation", "unary")] = routinginputs.Performance{SampleCount: 20, LatencyMS: 5, ObservedAt: now}
	options.Preferences = &Preferences{Strategy: ptr("latency")}
	plan, e = Select(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if e != nil || plan.Attempts[0].ProviderID != ids[1] {
		t.Fatalf("performance: %+v %v", plan, e)
	}
	options.Now = now.Add(61 * time.Second)
	plan, e = Select(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if e != nil {
		t.Fatal(e)
	}
	for _, decision := range plan.Decisions {
		if decision.Performance != nil {
			t.Fatal("stale metric used")
		}
	}
}

func TestDecisionPriceStrategyUsesTokenRates(t *testing.T) {
	s, slug, ids := planningFixture()
	route := s.Routes[slug]
	route.Operations = []string{"decision"}
	s.Routes[slug] = route
	for id, p := range s.Providers {
		p.Capabilities[0].Operation = "decision"
		s.Providers[id] = p
	}
	now := time.Now()
	inputs := &routinginputs.Inputs{RefreshedAt: now}
	for i, id := range ids[:2] {
		amount := []string{"2", "1"}[i]
		inputs.Prices = append(inputs.Prices, *rates.NewRoutingPrice(rates.RoutingPrice{Price: rates.Price{ProviderID: &id, ProviderKind: "openai", Model: "wire-model", Operation: "decision", InputPerMillion: &amount, OutputPerMillion: &amount, Currency: "USD"}, EffectiveAt: now.Add(-time.Hour)}))
	}
	if cheap, expensive := inputs.Prices[1].Scalar("decision"), inputs.Prices[0].Scalar("decision"); cheap == nil || expensive == nil || cheap.Cmp(expensive) >= 0 {
		t.Fatal("decision token rates did not produce ordered scalar prices")
	}
	plan, err := Select(&s, slug, "decision", "openai", "unary", []byte("seed"), Options{Inputs: inputs, Now: now, Preferences: &Preferences{Strategy: ptr("price")}})
	if err != nil || len(plan.Attempts) != 3 || plan.Attempts[0].ProviderID != ids[1] || plan.Attempts[2].ProviderID != ids[2] {
		t.Fatalf("decision token-price order: %+v %v", plan, err)
	}
}

func TestThroughputStrategyOrdersProvidersByMeasuredThroughput(t *testing.T) {
	snapshot, slug, ids := planningFixture()
	now := time.Now()
	inputs := &routinginputs.Inputs{
		RefreshedAt: now,
		Performance: map[string]routinginputs.Performance{
			routinginputs.PerformanceKey(ids[0], "wire-model", "generation", "unary"): {SampleCount: 20, LatencyMS: 5, Throughput: ptr(10.0), ObservedAt: now},
			routinginputs.PerformanceKey(ids[1], "wire-model", "generation", "unary"): {SampleCount: 20, LatencyMS: 5, Throughput: ptr(20.0), ObservedAt: now},
		},
	}
	plan, err := Select(&snapshot, slug, "generation", "openai", "unary", []byte("seed"), Options{
		Inputs: inputs, Now: now, Preferences: &Preferences{Strategy: ptr("throughput")},
	})
	if err != nil || len(plan.Attempts) != 3 || plan.Attempts[0].ProviderID != ids[1] || plan.Attempts[1].ProviderID != ids[0] || plan.Attempts[2].ProviderID != ids[2] {
		t.Fatalf("throughput order: %+v %v", plan, err)
	}
}

func TestLatencyStrategyOrdersProvidersByMeasuredLatency(t *testing.T) {
	snapshot, slug, _ := planningFixture()
	now := time.Now()
	weighted, err := Select(&snapshot, slug, "generation", "openai", "unary", []byte("seed"), Options{Now: now})
	if err != nil || len(weighted.Attempts) != 3 {
		t.Fatalf("weighted plan: %+v %v", weighted, err)
	}
	slow, fast, unknown := weighted.Attempts[0].ProviderID, weighted.Attempts[1].ProviderID, weighted.Attempts[2].ProviderID
	inputs := &routinginputs.Inputs{
		RefreshedAt: now,
		Performance: map[string]routinginputs.Performance{
			routinginputs.PerformanceKey(slow, "wire-model", "generation", "unary"): {SampleCount: 20, LatencyMS: 10, ObservedAt: now},
			routinginputs.PerformanceKey(fast, "wire-model", "generation", "unary"): {SampleCount: 20, LatencyMS: 5, ObservedAt: now},
		},
	}
	plan, err := Select(&snapshot, slug, "generation", "openai", "unary", []byte("seed"), Options{
		Inputs: inputs, Now: now, Preferences: &Preferences{Strategy: ptr("latency")},
	})
	if err != nil || len(plan.Attempts) != 3 || plan.Attempts[0].ProviderID != fast || plan.Attempts[1].ProviderID != slow || plan.Attempts[2].ProviderID != unknown {
		t.Fatalf("latency order: %+v %v", plan, err)
	}
}

// plan answers each decision's outcome by credential slot, or by provider
// for a target with no slot to try: its attempt, or its reason.

func TestPublishedRoutingOwnsConfigurationAndReturnedValues(t *testing.T) {
	snapshot, slug, ids := planningFixture()
	provider := snapshot.Providers[ids[0]]
	provider.Models = map[string]json.RawMessage{"wire-model": json.RawMessage(`{"context_length":2048,"max_output_tokens":1024,"supported_parameters":["response_format"]}`)}
	snapshot.Providers[ids[0]] = provider
	published, err := snapshot.CompiledView()
	if err != nil {
		t.Fatal(err)
	}
	options := Options{Preferences: &Preferences{Only: []string{"provider:" + ids[0]}}, TokenDemand: &TokenDemand{EstimatedInputTokens: 512}}
	before, err := Select(published, slug, "generation", "openai", "unary", []byte("seed"), options)
	if err != nil || len(before.Attempts) != 1 {
		t.Fatalf("initial plan %+v, error %v", before, err)
	}
	snapshot.Providers[ids[0]].Models["wire-model"] = json.RawMessage(`{"context_length":1}`)
	snapshot.Routes[slug].Targets[0].ProviderID = "missing"
	borrowed := published.Provider(ids[0])
	borrowed.Models["wire-model"] = json.RawMessage(`{"context_length":1}`)
	borrowed.Capabilities[0].Operation = "unsupported"
	facts, ok := borrowed.Metadata("wire-model")
	if !ok {
		t.Fatal("published model facts are unavailable")
	}
	*facts.ContextLength = 1
	published.Route(slug).Targets[0].ProviderID = "missing"
	after, err := Select(published, slug, "generation", "openai", "unary", []byte("seed"), options)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("borrowed values changed published routing, plan %+v, error %v", after, err)
	}
}
