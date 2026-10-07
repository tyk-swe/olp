package routes

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
)

// explainSnapshot publishes one route per name, each serving one model with
// the given context window.
func explainSnapshot(windows map[string]int) *runtime.Snapshot {
	credential := uuid.NewString()
	snapshot := &runtime.Snapshot{Providers: map[string]runtime.Provider{}, Routes: map[string]runtime.Route{}}
	for slug, window := range windows {
		provider, model := uuid.NewString(), slug+"-model"
		snapshot.Providers[provider] = runtime.Provider{
			ID: provider, Kind: "openai", Enabled: true, AuthMode: "api_key",
			Capabilities: []runtime.Capability{{Model: model, Operation: "generation", Surface: "openai", Mode: "unary"}},
			Models:       map[string]json.RawMessage{model: json.RawMessage(`{"context_length":` + itoa(window) + `}`)},
			Slots:        []runtime.Slot{{ID: uuid.NewString(), Enabled: true, Weight: 1, CredentialID: &credential}},
		}
		target := runtime.PublishedTarget{ID: uuid.NewString(), ProviderID: provider, ProviderModelID: uuid.NewString(), ProviderModel: model, Weight: 1}
		snapshot.Routes[slug] = simulationRoute(uuid.NewString(), slug, runtime.RouteFidelity{Mode: runtime.FidelityTransformed}, []string{"generation"}, 3000, 2, []runtime.PublishedTarget{target})
	}
	return snapshot
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func explainRequest(t *testing.T, snapshot *runtime.Snapshot, slug string, input simulationInput) explanation {
	t.Helper()
	input.Operation, input.Surface, input.Mode = "generation", "openai", "unary"
	demand, err := tokenDemand(input.EstimatedInputTokens, input.MaxOutputTokens)
	if err != nil {
		t.Fatal(err)
	}
	explained, err := (&Server{Access: &access.Server{}}).explain(fixedSimulation(snapshot, input, demand), slug)
	if err != nil {
		t.Fatal(err)
	}
	return explained
}

func legRoutes(e explanation) []string {
	var out []string
	for _, leg := range e.Legs {
		out = append(out, leg.Route)
	}
	return out
}

func TestSimulationFollowsPlanTimeFallbacksAndNamesTheStandbys(t *testing.T) {
	snapshot := explainSnapshot(map[string]int{"assistant": 200, "assistant-long": 100000, "assistant-backup": 200})
	primary := snapshot.Routes["assistant"]
	primary.Fallbacks = []runtime.Fallback{
		{Route: "assistant-long", On: []string{runtime.FallbackContextWindow}},
		{Route: "assistant-backup", On: []string{runtime.FallbackExhausted}},
		{Route: "assistant-retired", On: []string{runtime.FallbackExhausted}},
	}
	snapshot.Routes["assistant"] = primary

	short := explainRequest(t, snapshot, "assistant", simulationInput{EstimatedInputTokens: new(int64(50))})
	if got := legRoutes(short); !slices.Equal(got, []string{"assistant"}) {
		t.Fatalf("legs = %v, want only the named route", got)
	}
	outcomes := func(e explanation) []string {
		var out []string
		for _, step := range e.Fallbacks {
			out = append(out, step.Route+":"+step.Outcome)
		}
		return out
	}
	if got := outcomes(short); !slices.Equal(got, []string{"assistant-long:standby", "assistant-backup:standby", "assistant-retired:fallback_route_unavailable"}) {
		t.Fatalf("standby fallbacks = %v", got)
	}

	long := explainRequest(t, snapshot, "assistant", simulationInput{EstimatedInputTokens: new(int64(5000))})
	if got := legRoutes(long); !slices.Equal(got, []string{"assistant", "assistant-long"}) {
		t.Fatalf("legs = %v, want the long-context fallback after the named route", got)
	}
	if long.Legs[1].Via == nil || *long.Legs[1].Via != "fallback" || len(long.Legs[1].Decisions) != 1 || long.Legs[1].Decisions[0].Attempt == nil {
		t.Fatalf("fallback leg = %+v", long.Legs[1])
	}
	// The context-window fallback plans; the exhaustion fallbacks still stand
	// by behind it, in the order a failed attempt would reach them.
	if got := outcomes(long); !slices.Equal(got, []string{"assistant-long:planned", "assistant-backup:standby", "assistant-retired:fallback_route_unavailable"}) {
		t.Fatalf("fallbacks = %v", got)
	}
	if step := long.Fallbacks[0]; step.From != "assistant" || !slices.Equal(step.Conditions, []string{runtime.FallbackContextWindow}) {
		t.Fatalf("planned step = %+v", step)
	}
}

func TestSimulationExplainsSelectorDelegationAndSessionAffinity(t *testing.T) {
	snapshot := explainSnapshot(map[string]int{"assistant": 200000, "assistant-tools": 200000})
	primary := snapshot.Routes["assistant"]
	primary.Selectors = []runtime.Selector{
		{ID: "triage", When: runtime.Predicate{Classifier: &runtime.ClassifierPredicate{Route: "classifier", Labels: []string{"complex"}}}, Route: "assistant-tools"},
		{ID: "tools", When: runtime.Predicate{Tools: new(true)}, Route: "assistant-tools"},
	}
	primary.Affinity = &runtime.Affinity{Source: runtime.AffinityCacheKey}
	snapshot.Routes["assistant"] = primary
	tools := `{"model":"assistant","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"prompt_cache_key":"session-7"}`

	explained := explainRequest(t, snapshot, "assistant", simulationInput{Request: json.RawMessage(tools)})
	named := explained.Legs[0]
	if len(named.Selectors) != 2 || named.Selectors[0].Outcome != "classifier_not_simulated" || named.Selectors[1].Outcome != "matched" {
		t.Fatalf("selector trace = %+v", named.Selectors)
	}
	if got := legRoutes(explained); !slices.Equal(got, []string{"assistant", "assistant-tools"}) || *explained.Legs[1].Via != "selector" {
		t.Fatalf("legs = %v, want the delegated route", got)
	}
	if named.Affinity == nil || named.Affinity.Source != runtime.AffinityCacheKey || !named.Affinity.Session {
		t.Fatalf("affinity = %+v, want the cache key to seed the session", named.Affinity)
	}

	// A supplied classifier label decides the first selector.
	labelled := explainRequest(t, snapshot, "assistant", simulationInput{Request: json.RawMessage(tools), ClassifierLabels: map[string]string{"triage": "complex"}})
	if trace := labelled.Legs[0].Selectors; len(trace) != 1 || trace[0].Outcome != "matched" || trace[0].Label == nil || *trace[0].Label != "complex" {
		t.Fatalf("labelled trace = %+v", trace)
	}
}
