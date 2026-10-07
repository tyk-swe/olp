package runtime

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBehaviorValidationRefusesMalformedDeclarations(t *testing.T) {
	tagged := func(tag string) bool { return tag == "fast" }
	valid := Behavior{
		Fallbacks: []Fallback{{Route: "backup", On: []string{FallbackExhausted, FallbackContextWindow}}},
		Selectors: []Selector{{ID: "short", When: Predicate{MaxInputTokens: ptr(int64(500))}, Tags: []string{"fast"}}, {ID: "long", Route: "long-context"}},
		Retry:     Retry{"rate_limit": {MaxRetries: 2, BaseBackoffMS: 100, MaxBackoffMS: 1000, RespectRetryAfter: true}},
		Affinity:  &Affinity{Source: AffinityLabel, Label: "session"},
		Budget:    &CostLimits{DailyCostLimit: ptr("12.50")},
	}
	if err := valid.Validate("assistant", tagged); err != nil {
		t.Fatalf("valid behavior refused: %v", err)
	}
	if err := (CostLimits{DailyCostLimit: ptr("0.000000000001"), MonthlyCostLimit: ptr("0.000000000001")}).Validate("budget"); err != nil {
		t.Fatalf("small positive caps refused: %v", err)
	}
	for name, mutate := range map[string]func(*Behavior){
		"self fallback":      func(b *Behavior) { b.Fallbacks[0].Route = "assistant" },
		"duplicate fallback": func(b *Behavior) { b.Fallbacks = append(b.Fallbacks, b.Fallbacks[0]) },
		"unknown condition":  func(b *Behavior) { b.Fallbacks[0].On = []string{"anything"} },
		"no conditions":      func(b *Behavior) { b.Fallbacks[0].On = nil },
		"untagged selector":  func(b *Behavior) { b.Selectors[0].Tags = []string{"slow"} },
		"tags and route":     func(b *Behavior) { b.Selectors[0].Route = "backup" },
		"duplicate selector": func(b *Behavior) { b.Selectors[1].ID = "short" },
		"inverted bounds":    func(b *Behavior) { b.Selectors[0].When.MinInputTokens = ptr(int64(900)) },
		"unknown modality":   func(b *Behavior) { b.Selectors[0].When.Modalities = []string{"smell"} },
		"classifier self": func(b *Behavior) {
			b.Selectors[0].When.Classifier = &ClassifierPredicate{Route: "assistant", Labels: []string{"x"}, TimeoutMS: 100}
		},
		"classifier timeout": func(b *Behavior) {
			b.Selectors[0].When.Classifier = &ClassifierPredicate{Route: "tei", Labels: []string{"x"}}
		},
		"plugin identity":  func(b *Behavior) { b.Selectors[0].When.Plugin = &PluginPredicate{Digest: "not-a-digest"} },
		"credential retry": func(b *Behavior) { b.Retry["credential"] = RetryRule{MaxRetries: 1, BaseBackoffMS: 1, MaxBackoffMS: 1} },
		"retry ceiling": func(b *Behavior) {
			b.Retry["rate_limit"] = RetryRule{MaxRetries: 11, BaseBackoffMS: 1, MaxBackoffMS: 1}
		},
		"inverted backoff": func(b *Behavior) {
			b.Retry["rate_limit"] = RetryRule{MaxRetries: 1, BaseBackoffMS: 10, MaxBackoffMS: 1}
		},
		"cache key label":       func(b *Behavior) { b.Affinity = &Affinity{Source: AffinityCacheKey, Label: "session"} },
		"unlabelled affinity":   func(b *Behavior) { b.Affinity = &Affinity{Source: AffinityLabel} },
		"empty budget":          func(b *Behavior) { b.Budget = &CostLimits{} },
		"inexact budget":        func(b *Behavior) { b.Budget = &CostLimits{MonthlyCostLimit: ptr("1e3")} },
		"zero daily budget":     func(b *Behavior) { b.Budget = &CostLimits{DailyCostLimit: ptr("0")} },
		"zero monthly budget":   func(b *Behavior) { b.Budget = &CostLimits{MonthlyCostLimit: ptr("00.000000000000")} },
		"selector without verb": func(b *Behavior) { b.Selectors[1].Route = "" },
	} {
		t.Run(name, func(t *testing.T) {
			b := valid
			b.Fallbacks = append([]Fallback(nil), valid.Fallbacks...)
			b.Selectors = append([]Selector(nil), valid.Selectors...)
			b.Retry = Retry{}
			for class, rule := range valid.Retry {
				b.Retry[class] = rule
			}
			mutate(&b)
			if err := b.Validate("assistant", tagged); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if _, err := DecodeBehavior([]byte(`{"fallbacks":[],"surprise":1}`)); err == nil {
		t.Fatal("decoded an unknown field")
	}
	if b, err := DecodeBehavior([]byte(`{}`)); err != nil || !b.IsZero() {
		t.Fatalf("empty behavior: %+v %v", b, err)
	}
}

func TestRouteGraphRefusesCyclesDepthProjectsAndStrictToTransformed(t *testing.T) {
	project, other := ptr(uuid.NewString()), ptr(uuid.NewString())
	route := func(slug string, edges ...string) Route {
		r := Route{Slug: slug, ProjectID: project, Fidelity: RouteFidelity{Mode: FidelityTransformed}}
		for _, to := range edges {
			r.Fallbacks = append(r.Fallbacks, Fallback{Route: to, On: []string{FallbackExhausted}})
		}
		return r
	}
	graph := func(routes ...Route) map[string]Route {
		out := map[string]Route{}
		for _, r := range routes {
			out[r.Slug] = r
		}
		return out
	}
	code := func(err error) string {
		var refusal *Refusal
		if errors.As(err, &refusal) {
			return refusal.Code
		}
		return ""
	}
	if err := CheckRouteGraph(graph(route("a", "b", "missing"), route("b", "c"), route("c"))); err != nil {
		t.Fatalf("three routes deep refused: %v", err)
	}
	if got := code(CheckRouteGraph(graph(route("a", "b"), route("b", "c"), route("c", "d"), route("d")))); got != "route_graph_too_deep" {
		t.Fatalf("four routes deep: %q", got)
	}
	if got := code(CheckRouteGraph(graph(route("a", "b"), route("b", "a")))); got != "route_graph_cycle" {
		t.Fatalf("cycle: %q", got)
	}
	foreign := route("b")
	foreign.ProjectID = other
	if got := code(CheckRouteGraph(graph(route("a", "b"), foreign))); got != "route_graph_project_mismatch" {
		t.Fatalf("project: %q", got)
	}
	strict := route("a", "b")
	strict.Fidelity = RouteFidelity{Mode: FidelityStrict}
	if got := code(CheckRouteGraph(graph(strict, route("b")))); got != "route_graph_fidelity_mismatch" {
		t.Fatalf("strict to transformed: %q", got)
	}
	// A classifier only labels the request, so a strict route may classify
	// with a transformed one, and the classifier adds no serving depth.
	classified := route("a", "b")
	classified.Fidelity = RouteFidelity{Mode: FidelityStrict}
	classified.Fallbacks = nil
	classified.Selectors = []Selector{{ID: "s", When: Predicate{Classifier: &ClassifierPredicate{Route: "tei", Labels: []string{"x"}, TimeoutMS: 10}}, Tags: []string{"t"}}}
	if err := CheckRouteGraph(graph(classified, route("tei", "x"), route("x", "y"), route("y"))); err != nil {
		t.Fatalf("classifier edge: %v", err)
	}
}

func TestRetryBackoffUsesFullJitterAndHonorsRetryAfter(t *testing.T) {
	rule := RetryRule{MaxRetries: 5, BaseBackoffMS: 100, MaxBackoffMS: 1000}
	for retry, ceiling := range map[int]time.Duration{1: 100 * time.Millisecond, 2: 200 * time.Millisecond, 4: 800 * time.Millisecond, 5: time.Second, 60: time.Second} {
		if got := rule.Backoff(retry, 0.999999, 0); got > ceiling || got < ceiling*99/100 {
			t.Fatalf("retry %d: %v, want near %v", retry, got, ceiling)
		}
		if got := rule.Backoff(retry, 0, 0); got != 0 {
			t.Fatalf("jitter floor %v", got)
		}
	}
	if got := rule.Backoff(1, 0.5, 3*time.Second); got != 50*time.Millisecond {
		t.Fatalf("ignored Retry-After should not apply: %v", got)
	}
	rule.RespectRetryAfter = true
	if got := rule.Backoff(1, 0.5, 3*time.Second); got != 3*time.Second {
		t.Fatalf("Retry-After: %v", got)
	}
}

func TestShadowSamplingIsDeterministicAndProportional(t *testing.T) {
	target := uuid.NewString()
	hits := 0
	for i := 0; i < 10000; i++ {
		seed := []byte(uuid.NewString())
		sampled := Sampled(target, seed, 0.25)
		if sampled != Sampled(target, seed, 0.25) {
			t.Fatal("sampling is not a function of its seed")
		}
		if sampled {
			hits++
		}
	}
	if hits < 2200 || hits > 2800 {
		t.Fatalf("sampled %d of 10000 at 25%%", hits)
	}
	if !Sampled(target, nil, 1) {
		t.Fatal("full rate skipped a request")
	}
}

func TestPredicatesMatchTheirFeatureConjunction(t *testing.T) {
	features := Features{Operation: "generation", InputTokens: 1200, OutputTokens: ptr(int64(64)), Streaming: true, Tools: true, Modalities: []string{"text", "image"}, ReasoningEffort: "high"}
	for name, tc := range map[string]struct {
		predicate Predicate
		want      bool
	}{
		"empty":            {Predicate{}, true},
		"operation":        {Predicate{Operations: []string{"embeddings"}}, false},
		"input window":     {Predicate{MinInputTokens: ptr(int64(1000)), MaxInputTokens: ptr(int64(2000))}, true},
		"input too long":   {Predicate{MaxInputTokens: ptr(int64(1000))}, false},
		"output bound":     {Predicate{MaxOutputTokens: ptr(int64(32))}, false},
		"any modality":     {Predicate{Modalities: []string{"audio", "image"}}, true},
		"absent modality":  {Predicate{Modalities: []string{"audio"}}, false},
		"streaming":        {Predicate{Streaming: ptr(false)}, false},
		"tools":            {Predicate{Tools: ptr(true)}, true},
		"structured":       {Predicate{StructuredOutput: ptr(true)}, false},
		"reasoning effort": {Predicate{ReasoningEffort: []string{"medium", "high"}}, true},
	} {
		if got := tc.predicate.Matches(features); got != tc.want {
			t.Errorf("%s: matched %v", name, got)
		}
	}
	unbounded := features
	unbounded.OutputTokens = nil
	if (Predicate{MinOutputTokens: ptr(int64(1))}).Matches(unbounded) {
		t.Fatal("an output bound matched a request without one")
	}
}
