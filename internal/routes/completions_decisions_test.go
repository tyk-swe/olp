package routes

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestCompletionInspectionPricesAggregateExposure(t *testing.T) {
	snapshot := strictEstimateSnapshot("gpt-4o")
	for id, p := range snapshot.Providers {
		p.Kind, p.VendorID, p.ProfileID = "openai", "openai", "openai-completions"
		snapshot.Providers[id] = p
	}
	body, _ := json.Marshal(map[string]any{"model": "route", "prompt": [][]int{make([]int, 50), make([]int, 20)}, "n": 3, "max_tokens": 10})
	input := simulationInput{Operation: "generation", Surface: "openai", Mode: "unary", Dialect: "openai-completions", Request: body}
	m := fixedSimulation(snapshot, input, nil)
	price := usage.RoutingPrice{Price: usage.Price{ProviderKind: "openai", Model: "gpt-4o", Operation: "generation", InputPerMillion: new("1"), OutputPerMillion: new("1")}}
	m.inputs = &usage.RoutingInputs{Prices: []usage.RoutingPrice{price}, RefreshedAt: time.Now()}
	leg, _, _, err := (&Server{Access: &access.Server{}}).leg(m, "route", "")
	if err != nil {
		t.Fatal(err)
	}
	d := decisionFor(t, leg.Decisions, "gpt-4o")
	parsed, err := protocols.Parse(wire.Completions, body, "")
	if err != nil {
		t.Fatal(err)
	}
	e := estimate.Walk(parsed).Estimate(estimate.ForModel("gpt-4o"), nil)
	want, ok := price.CostBound(e.InputExposure(), e.Reply(), true, time.Now(), nil)
	if !ok || !d.Eligible || d.EstimatedCost == nil || d.EstimatedCost.Maximum != want.String() || d.EstimatedInputTokens == nil || *d.EstimatedInputTokens != e.ContextInput {
		t.Fatalf("aggregate pricing/context mismatch: %+v; want cost %s and context %d", d, want.String(), e.ContextInput)
	}
}

func TestCompletionAutomaticInspectionUsesTargetContext(t *testing.T) {
	snapshot := strictEstimateSnapshot("gpt-4o")
	route := snapshot.Routes["route"]
	route.Fidelity.Mode = routing.FidelityTransformed
	snapshot.Routes["route"] = route
	for id, p := range snapshot.Providers {
		p.Kind, p.VendorID, p.ProfileID = "openai", "openai", ""
		p.Models = map[string]json.RawMessage{"gpt-4o": json.RawMessage(`{"context_length":7}`)}
		snapshot.Providers[id] = p
	}
	input := simulationInput{Operation: "generation", Surface: "openai", Mode: "unary", Dialect: "openai-completions", Request: json.RawMessage(`{"model":"route","prompt":"hello","max_tokens":1}`)}
	leg, _, _, err := (&Server{Access: &access.Server{}}).leg(fixedSimulation(snapshot, input, nil), "route", "")
	if err != nil {
		t.Fatal(err)
	}
	d := decisionFor(t, leg.Decisions, "gpt-4o")
	if d.Eligible || d.Reason == nil || *d.Reason != "context_length_exceeded" {
		t.Fatalf("mapped context not checked: %+v", d)
	}
}

func TestDecisionInspectionRejectsUnboundedBudgets(t *testing.T) {
	for _, scope := range []string{"none", "provider", "slot", "unrelated_slot", "key", "attribution", "key_default", "project_default"} {
		t.Run(scope, func(t *testing.T) {
			snapshot, _ := estimateSnapshot("model")
			route := snapshot.Routes["route"]
			route.Operations = []string{"decision"}
			snapshot.Routes["route"] = route
			for id, p := range snapshot.Providers {
				p.Kind, p.VendorID, p.ProfileID, p.ProfileRevision = "openai", "openai", "openai-decisions", "1"
				p.Capabilities = []routing.Capability{{Model: "model", Operation: "decision", Surface: "openai", Mode: "unary"}}
				if scope == "provider" {
					p.Limits = &routing.Limits{TokensPerMinute: new(int64(1000))}
				}
				if scope == "slot" {
					p.Slots[0].TokensPerMinute = new(int64(1000))
				}
				if scope == "unrelated_slot" {
					slot := p.Slots[0]
					slot.AllowedModels = []string{"other"}
					slot.TokensPerMinute = new(int64(1000))
					p.Slots = append(p.Slots, slot)
				}
				snapshot.Providers[id] = p
			}
			input := simulationInput{Operation: "decision", Surface: "openai", Mode: "unary", Dialect: "openai-decisions", Request: json.RawMessage(`{"model":"route","input":"hello","questions":[{"type":"predicate","instructions":"Is this text?"}]}`)}
			m := fixedSimulation(snapshot, input, nil)
			if scope == "key" {
				m.key = func(routing.Route) (inspectionKeyContext, error) {
					return inspectionKeyContext{authority: access.Authority{Policy: access.KeyPolicy{TokensPerMinute: new(int64(1000))}}}, nil
				}
			}
			if scope == "attribution" || scope == "key_default" || scope == "project_default" {
				a := access.Authority{ProjectAttributionBudgets: access.AttributionBudgets{"team": {"core": {DailyCostLimit: new("1")}}}}
				switch scope {
				case "attribution":
					m.input.Attribution = map[string]string{"team": "core"}
				case "key_default":
					a.Policy.AttributionDefaults = map[string]string{"team": "core"}
				case "project_default":
					a.ProjectAttributionPolicy = attribution.Policy{Defaults: map[string]string{"team": "core"}}
				}
				m.key = func(routing.Route) (inspectionKeyContext, error) { return inspectionKeyContext{authority: a}, nil }
			}
			leg, _, _, err := (&Server{Access: &access.Server{}}).leg(m, "route", "")
			if err != nil {
				t.Fatal(err)
			}
			d := decisionFor(t, leg.Decisions, "model")
			want := scope == "none" || scope == "unrelated_slot"
			if d.Eligible != want || !want && (d.Incompatibility == nil || d.Incompatibility.Code != "unbounded_work_budget") {
				t.Fatalf("inspection budget %s: %+v", scope, d)
			}
		})
	}
}
