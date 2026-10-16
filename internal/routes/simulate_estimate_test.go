package routes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
)

// estimateSnapshot publishes a route with one target for each model, every one
// with a context window of two hundred tokens.
func estimateSnapshot(models ...string) (*routing.Snapshot, []string) {
	credential := uuid.NewString()
	snapshot := &routing.Snapshot{Providers: map[string]routing.Provider{}, Routes: map[string]routing.Route{}}
	var targets []routing.PublishedTarget
	var ids []string
	for i, model := range models {
		id, modelID := uuid.NewString(), uuid.NewString()
		targets = append(targets, routing.PublishedTarget{ID: uuid.NewString(), ProviderID: id, ProviderModelID: modelID, ProviderModel: model, Priority: i, Weight: 1})
		ids = append(ids, targets[i].ID)
		snapshot.Providers[id] = routing.Provider{
			ID: id, Kind: "openai", Enabled: true, AuthMode: "api_key",
			Capabilities: []routing.Capability{{Model: model, Operation: "generation", Surface: "openai", Mode: "unary"}},
			Models:       map[string]json.RawMessage{model: json.RawMessage(`{"context_length":200}`)},
			Slots:        []routing.Slot{{ID: uuid.NewString(), Enabled: true, Weight: 1, CredentialID: &credential}},
		}
	}
	snapshot.Routes["route"] = simulationRoute(uuid.NewString(), "route", routing.RouteFidelity{Mode: routing.FidelityTransformed}, []string{"generation"}, 3000, 4, targets)
	return snapshot, ids
}

func simulate(t *testing.T, snapshot *routing.Snapshot, input simulationInput) []inspectedDecision {
	t.Helper()
	input.Operation, input.Surface, input.Mode = "generation", "openai", "unary"
	demand, err := tokenDemand(input.EstimatedInputTokens, input.MaxOutputTokens)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Access: &access.Server{}}
	leg, _, _, err := s.leg(fixedSimulation(snapshot, input, demand), "route", "")
	if err != nil {
		t.Fatal(err)
	}
	return leg.Decisions
}

func decisionFor(t *testing.T, decisions []inspectedDecision, model string) inspectedDecision {
	t.Helper()
	for _, d := range decisions {
		if d.UpstreamModel == model {
			return d
		}
	}
	t.Fatalf("no decision for %s in %+v", model, decisions)
	return inspectedDecision{}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// strictEstimateSnapshot is estimateSnapshot with a strict route: every target
// is a provider on the compatible chat profile, which prepares the request the
// target is sent from the caller's.
func strictEstimateSnapshot(models ...string) *routing.Snapshot {
	snapshot, _ := estimateSnapshot(models...)
	for id, provider := range snapshot.Providers {
		provider.Kind, provider.ProfileID, provider.ProfileRevision = "openai_compatible", "compatible-chat", "1"
		snapshot.Providers[id] = provider
	}
	route := snapshot.Routes["route"]
	route.Fidelity = routing.RouteFidelity{Mode: routing.FidelityStrict}
	snapshot.Routes["route"] = route
	return snapshot
}

func simulateStrict(t *testing.T, snapshot *routing.Snapshot, input simulationInput) []inspectedDecision {
	t.Helper()
	input.Dialect = "openai-chat"
	return simulate(t, snapshot, input)
}

// TestStrictSimulationWeighsTheRequestTheTargetIsSent covers the route that
// prepares a request of its own for each target. Its window is checked against
// that request, counted for the target's model: the framing of an OpenAI chat
// message is counted for the OpenAI model and not for the other, and the
// decision says how, as it does on a transformed route.
func TestStrictSimulationWeighsTheRequestTheTargetIsSent(t *testing.T) {
	snapshot := strictEstimateSnapshot("gpt-4o", "claude-sonnet-4-5")
	// A hundred and one tokens of o200k_base, which are a hundred and eight with
	// the message framed, and six hundred characters, a hundred and fifty to the
	// four-character rule.
	body := `{"model":"route","max_tokens":5,"messages":[{"role":"user","content":"` + strings.Repeat("hello ", 100) + `"}]}`
	decisions := simulateStrict(t, snapshot, simulationInput{Request: json.RawMessage(body)})

	for model, want := range map[string]struct {
		input      int64
		provenance string
		family     string
	}{
		"gpt-4o":            {108, "tokenizer", "openai-o200k"},
		"claude-sonnet-4-5": {150, "heuristic", "anthropic"},
	} {
		d := decisionFor(t, decisions, model)
		if d.Interaction == nil || d.Interaction.Status != "admitted" {
			t.Fatalf("%s: the strict target was not prepared: %+v", model, d.Interaction)
		}
		if d.EstimatedInputTokens == nil || *d.EstimatedInputTokens != want.input {
			t.Errorf("%s: estimated input %v, want %d", model, deref(d.EstimatedInputTokens), want.input)
		}
		if d.EstimateProvenance == nil || *d.EstimateProvenance != want.provenance || d.ModelFamily == nil || *d.ModelFamily != want.family {
			t.Errorf("%s: provenance %v family %v, want %s for %s", model, deref(d.EstimateProvenance), deref(d.ModelFamily), want.provenance, want.family)
		}
		if d.RequestedOutputTokens == nil || *d.RequestedOutputTokens != 5 || !d.Eligible {
			t.Errorf("%s: requested output %v, eligible %v (%v), want the request's own bound of 5 to fit", model, deref(d.RequestedOutputTokens), d.Eligible, deref(d.Reason))
		}
	}

	// The request's window verdict follows its count: the same prompt in a
	// script the OpenAI tokenizer charges more for fills the window.
	body = `{"model":"route","max_tokens":5,"messages":[{"role":"user","content":"` + strings.Repeat("日本語で", 100) + `"}]}`
	decisions = simulateStrict(t, snapshot, simulationInput{Request: json.RawMessage(body)})
	if d := decisionFor(t, decisions, "gpt-4o"); d.Eligible || d.Reason == nil || *d.Reason != "context_length_exceeded" {
		t.Errorf("gpt-4o with 307 tokens against a window of 200: eligible %v (%v)", d.Eligible, deref(d.Reason))
	}
	if d := decisionFor(t, decisions, "claude-sonnet-4-5"); !d.Eligible {
		t.Errorf("claude with 100 tokens against a window of 200: %v", deref(d.Reason))
	}
}

// The request's own five tokens of reply fit; the caller's hundred do not.

// A caller's input estimate stands for every target, with the bound it names.

// fixedSimulation simulates without a selected key and with every credential
// eligible.
func fixedSimulation(snapshot *routing.Snapshot, input simulationInput, demand *routing.TokenDemand) *simulation {
	m := (&Server{}).newSimulation(context.Background(), snapshot, input, nil, demand)
	m.key = func(routing.Route) (inspectionKeyContext, error) { return inspectionKeyContext{}, nil }
	m.eligibility = func(routing.Route) (func(string) routing.Eligibility, error) {
		return func(string) routing.Eligibility { return routing.Eligible }, nil
	}
	return m
}
