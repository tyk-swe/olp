package routing

import (
	"testing"

	"github.com/google/uuid"
)

func TestGatewayToolExecutionBehaviorValidatesAndRoundTrips(t *testing.T) {
	toolsetID := uuid.NewString()
	valid, err := DecodeBehavior([]byte(`{"tool_execution":"gateway","toolsets":["` + toolsetID + `"]}`))
	if err != nil || valid.IsZero() || valid.ToolExecution != "gateway" || len(valid.Toolsets) != 1 || valid.Toolsets[0] != toolsetID {
		t.Fatalf("workflow behavior was not retained: %+v, %v", valid, err)
	}
	if err := valid.Validate("route", func(string) bool { return false }); err != nil {
		t.Fatalf("valid workflow behavior refused: %v", err)
	}
	for _, tc := range []struct {
		name     string
		behavior Behavior
	}{
		{"unknown execution mode", Behavior{ToolExecution: "other"}},
		{"toolsets without gateway execution", Behavior{Toolsets: []string{toolsetID}}},
		{"malformed toolset", Behavior{ToolExecution: "gateway", Toolsets: []string{"not-a-uuid"}}},
		{"duplicate toolset", Behavior{ToolExecution: "gateway", Toolsets: []string{toolsetID, toolsetID}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.behavior.Validate("route", func(string) bool { return false }); err == nil {
				t.Fatal("invalid workflow behavior was accepted")
			}
		})
	}
}

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

// A classifier only labels the request, so a strict route may classify
// with a transformed one, and the classifier adds no serving depth.
