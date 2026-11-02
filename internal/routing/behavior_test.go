package routing

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestSelectorIdentifiersAgreeWithAccounting(t *testing.T) {
	for _, tc := range []struct {
		id    string
		valid bool
	}{
		{"selector", true}, {"selector-1", true}, {"UPPER", false}, {"with_underscore", false},
		{"", false}, {strings.Repeat("a", 63), true}, {strings.Repeat("a", 64), false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			behavior := Behavior{Selectors: []Selector{{ID: tc.id, Route: "destination"}}}
			behaviorErr := behavior.Validate("source", nil)
			now := time.Now().UTC()
			event := &usage.Event{
				Version: usage.WireVersion, EventID: uuid.NewString(), RequestID: uuid.NewString(),
				RuntimeGenerationID: uuid.NewString(), APIKeyID: uuid.NewString(), RouteSlug: "source",
				Operation: "generation", Surface: "openai", RequestStartedAt: now, RequestCompletedAt: now, ObservedAt: now,
				Attempts: []usage.Attempt{{
					ID: uuid.NewString(), Ordinal: 1, ProviderID: uuid.NewString(), UpstreamModel: "model",
					StartedAt: now, CompletedAt: now,
					Routing: &usage.Routing{ProviderRevisionID: uuid.NewString(), Selector: &tc.id},
				}},
			}
			payload, err := usage.Encode(event)
			if err != nil {
				t.Fatal(err)
			}
			_, accountingErr := usage.Decode(payload)
			if (behaviorErr == nil) != tc.valid || (accountingErr == nil) != tc.valid {
				t.Fatalf("routing error = %v, accounting error = %v", behaviorErr, accountingErr)
			}
		})
	}
}

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

func TestModalitySelectorsAcceptAndMatchRequestFeatures(t *testing.T) {
	request, err := wire.FromFields(wire.Gemini, "route", false, map[string]json.RawMessage{
		"contents": json.RawMessage(`[{"role":"user","parts":[{"text":"hello"},{"inlineData":{"mimeType":"image/png","data":"YQ=="}},{"inlineData":{"mimeType":"audio/wav","data":"YQ=="}},{"inlineData":{"mimeType":"video/mp4","data":"YQ=="}},{"inlineData":{"mimeType":"application/pdf","data":"YQ=="}}]}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	features := DescribeRequest("generation", request, nil)
	for _, modality := range []string{"text", "image", "audio", "video", "file"} {
		t.Run(modality, func(t *testing.T) {
			predicate := Predicate{Modalities: []string{modality}}
			behavior := Behavior{Selectors: []Selector{{ID: "media", When: predicate, Route: "media-route"}}}
			if err := behavior.Validate("route", func(string) bool { return false }); err != nil {
				t.Fatalf("selector rejected: %v", err)
			}
			if !predicate.Matches(*features) {
				t.Fatalf("selector did not match request features %+v", features)
			}
		})
	}
}

// A classifier only labels the request, so a strict route may classify
// with a transformed one, and the classifier adds no serving depth.
