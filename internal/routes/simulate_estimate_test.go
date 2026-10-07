package routes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

// estimateSnapshot publishes a route with one target for each model, every one
// with a context window of two hundred tokens.
func estimateSnapshot(models ...string) (*runtime.Snapshot, []string) {
	credential := uuid.NewString()
	snapshot := &runtime.Snapshot{Providers: map[string]runtime.Provider{}, Routes: map[string]runtime.Route{}}
	var targets []runtime.PublishedTarget
	var ids []string
	for i, model := range models {
		id, modelID := uuid.NewString(), uuid.NewString()
		targets = append(targets, runtime.PublishedTarget{ID: uuid.NewString(), ProviderID: id, ProviderModelID: modelID, ProviderModel: model, Priority: i, Weight: 1})
		ids = append(ids, targets[i].ID)
		snapshot.Providers[id] = runtime.Provider{
			ID: id, Kind: "openai", Enabled: true, AuthMode: "api_key",
			Capabilities: []runtime.Capability{{Model: model, Operation: "generation", Surface: "openai", Mode: "unary"}},
			Models:       map[string]json.RawMessage{model: json.RawMessage(`{"context_length":200}`)},
			Slots:        []runtime.Slot{{ID: uuid.NewString(), Enabled: true, Weight: 1, CredentialID: &credential}},
		}
	}
	snapshot.Routes["route"] = simulationRoute(uuid.NewString(), "route", runtime.RouteFidelity{Mode: runtime.FidelityTransformed}, []string{"generation"}, 3000, 4, targets)
	return snapshot, ids
}

func simulate(t *testing.T, snapshot *runtime.Snapshot, input simulationInput) []inspectedDecision {
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

// TestSimulationEstimatesTheRequestForEachTarget sends a simulation a request
// whose text the four-character rule charges at a hundred tokens and o200k_base
// counts as three hundred and seven with its framing. The decisions say what
// admission would count for each target, how, and weigh each window by it.
func TestSimulationEstimatesTheRequestForEachTarget(t *testing.T) {
	snapshot, _ := estimateSnapshot("gpt-4o", "claude-sonnet-4-5", "mistral-large")
	body := `{"model":"route","max_tokens":5,"messages":[{"role":"user","content":"` + strings.Repeat("日本語で", 100) + `"}]}`
	decisions := simulate(t, snapshot, simulationInput{Request: json.RawMessage(body)})

	for model, want := range map[string]struct {
		input      int64
		provenance string
		family     string
		eligible   bool
	}{
		"gpt-4o":            {307, "tokenizer", "openai-o200k", false},
		"claude-sonnet-4-5": {100, "heuristic", "anthropic", true},
		"mistral-large":     {100, "heuristic", "other", true},
	} {
		d := decisionFor(t, decisions, model)
		if d.EstimatedInputTokens == nil || *d.EstimatedInputTokens != want.input {
			t.Errorf("%s: estimated input %v, want %d", model, deref(d.EstimatedInputTokens), want.input)
		}
		if d.EstimateProvenance == nil || *d.EstimateProvenance != want.provenance || d.ModelFamily == nil || *d.ModelFamily != want.family {
			t.Errorf("%s: provenance %v family %v, want %s for %s", model, deref(d.EstimateProvenance), deref(d.ModelFamily), want.provenance, want.family)
		}
		if d.RequestedOutputTokens == nil || *d.RequestedOutputTokens != 5 {
			t.Errorf("%s: requested output %v, want the request's own bound", model, deref(d.RequestedOutputTokens))
		}
		if d.Eligible != want.eligible {
			t.Errorf("%s: eligible %v (%v), want %v", model, d.Eligible, deref(d.Reason), want.eligible)
		}
	}
	if reason := decisionFor(t, decisions, "gpt-4o").Reason; reason == nil || *reason != "context_length_exceeded" {
		t.Errorf("the OpenAI target's reason %v, want context_length_exceeded", deref(reason))
	}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestSimulationHonorsWhatTheCallerSupplies keeps the estimate a caller names
// the one every target is weighed by, with no provenance to claim for it, and
// lets a named reply bound win over the request's.
func TestSimulationHonorsWhatTheCallerSupplies(t *testing.T) {
	snapshot, _ := estimateSnapshot("gpt-4o", "claude-sonnet-4-5")
	body := `{"model":"route","max_tokens":5,"messages":[{"role":"user","content":"` + strings.Repeat("日本語で", 100) + `"}]}`
	input, output := int64(50), int64(7)

	decisions := simulate(t, snapshot, simulationInput{Request: json.RawMessage(body), EstimatedInputTokens: &input})
	for _, d := range decisions {
		if d.EstimatedInputTokens == nil || *d.EstimatedInputTokens != 50 || d.EstimateProvenance != nil || d.ModelFamily != nil || !d.Eligible {
			t.Errorf("%s with a caller's estimate: %+v", d.UpstreamModel, d)
		}
		if d.RequestedOutputTokens != nil {
			t.Errorf("%s: requested output %v, want none named", d.UpstreamModel, deref(d.RequestedOutputTokens))
		}
	}

	decisions = simulate(t, snapshot, simulationInput{Request: json.RawMessage(body), MaxOutputTokens: &output})
	for _, d := range decisions {
		if d.RequestedOutputTokens == nil || *d.RequestedOutputTokens != 7 || d.EstimateProvenance == nil {
			t.Errorf("%s with a caller's reply bound: %+v", d.UpstreamModel, d)
		}
	}
}

// TestSimulationWithoutARequestEstimatesNothing keeps a tuple-only simulation
// as it was: no request is fabricated, so no estimate is made.
func TestSimulationWithoutARequestEstimatesNothing(t *testing.T) {
	snapshot, _ := estimateSnapshot("gpt-4o", "claude-sonnet-4-5")
	for _, request := range []json.RawMessage{nil, json.RawMessage(`{"route":"route"}`), json.RawMessage(`{"model":"route"}`)} {
		for _, d := range simulate(t, snapshot, simulationInput{Request: request}) {
			if d.EstimatedInputTokens != nil || d.EstimateProvenance != nil || d.ModelFamily != nil || !d.Eligible {
				t.Errorf("%s with request %s: %+v", d.UpstreamModel, request, d)
			}
		}
	}
}

// TestSimulationParsesRequestsAsTheGatewayDoes holds the two to one estimate:
// a body in a native dialect reaches the walker through the parser the gateway
// uses, so the number a simulation shows is the number admission counts.
func TestSimulationParsesRequestsAsTheGatewayDoes(t *testing.T) {
	for _, tc := range []struct {
		name, surface string
		family        openai.Family
		body          string
	}{
		{"chat", "openai", openai.FamilyChat, `{"model":"route","max_tokens":9,"n":2,"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`},
		{"anthropic", "anthropic", openai.FamilyAnthropic, `{"model":"route","max_tokens":9,"system":"Be brief.","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[{"name":"f","input_schema":{"type":"object"}}]}`},
		{"gemini", "gemini", openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"systemInstruction":{"parts":[{"text":"Be brief."}]},"generationConfig":{"maxOutputTokens":9,"candidateCount":2}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway, err := protocols.Parse(tc.family, []byte(tc.body), "route")
			if err != nil {
				t.Fatal(err)
			}
			simulated, err := inspectorRequest(json.RawMessage(tc.body), "generation", tc.surface, "unary", "", "route", false)
			if err != nil || simulated == nil {
				t.Fatalf("simulation parsed %v: %v", simulated, err)
			}
			for _, model := range []string{"gpt-4o", "gpt-4", "claude-sonnet-4-5", "gemini-2.5-pro", "unknown"} {
				c := estimate.ForModel(model)
				want, got := estimate.Walk(gateway).Estimate(c, nil), estimate.Walk(simulated).Estimate(c, nil)
				if got.Input != want.Input || got.Tokens() != want.Tokens() || got.Provenance != want.Provenance {
					t.Errorf("%s: simulation estimates %+v, the gateway %+v", model, got, want)
				}
			}
		})
	}
}

func TestSimulationParsesNativeGenerationDialects(t *testing.T) {
	for _, test := range []struct {
		dialect string
		family  openai.Family
		body    string
	}{
		{"mistral-fim", openai.FamilyMistralFIM, `{"prompt":"def f():","suffix":"\n"}`},
		{"cohere-chat-v2", openai.FamilyCohereChat, `{"messages":[{"role":"user","content":"hi"}]}`},
	} {
		for _, strict := range []bool{false, true} {
			parsed, err := inspectorRequest(json.RawMessage(test.body), "generation", "native", "unary", test.dialect, "route", strict)
			if err != nil || parsed == nil || parsed.Family != test.family {
				t.Fatalf("%s strict=%v: parsed %+v, %v", test.dialect, strict, parsed, err)
			}
		}
	}
}

// strictEstimateSnapshot is estimateSnapshot with a strict route: every target
// is a provider on the compatible chat profile, which prepares the request the
// target is sent from the caller's.
func strictEstimateSnapshot(models ...string) *runtime.Snapshot {
	snapshot, _ := estimateSnapshot(models...)
	for id, provider := range snapshot.Providers {
		provider.Kind, provider.ProfileID, provider.ProfileRevision = "openai_compatible", "compatible-chat", "1"
		snapshot.Providers[id] = provider
	}
	route := snapshot.Routes["route"]
	route.Fidelity = runtime.RouteFidelity{Mode: runtime.FidelityStrict}
	snapshot.Routes["route"] = route
	return snapshot
}

func simulateStrict(t *testing.T, snapshot *runtime.Snapshot, input simulationInput) []inspectedDecision {
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

// TestStrictSimulationHonorsTheReplyBoundTheCallerSupplies holds a strict route
// to what the documentation says of max_output_tokens on a simulation: it
// replaces the bound the request names, here as on a transformed route, and the
// window verdict follows it.
func TestStrictSimulationHonorsTheReplyBoundTheCallerSupplies(t *testing.T) {
	snapshot := strictEstimateSnapshot("gpt-4o", "claude-sonnet-4-5")
	body := `{"model":"route","max_tokens":5,"messages":[{"role":"user","content":"` + strings.Repeat("hello ", 100) + `"}]}`
	output, input := int64(100), int64(10)

	// The request's own five tokens of reply fit; the caller's hundred do not.
	decisions := simulateStrict(t, snapshot, simulationInput{Request: json.RawMessage(body), MaxOutputTokens: &output})
	for _, d := range decisions {
		if d.RequestedOutputTokens == nil || *d.RequestedOutputTokens != 100 {
			t.Errorf("%s: requested output %v, want the caller's 100", d.UpstreamModel, deref(d.RequestedOutputTokens))
		}
		if d.Eligible || d.Reason == nil || *d.Reason != "context_length_exceeded" {
			t.Errorf("%s: eligible %v (%v), want the caller's bound to overflow the window", d.UpstreamModel, d.Eligible, deref(d.Reason))
		}
		if d.EstimateProvenance == nil {
			t.Errorf("%s: no provenance for the count of the request it is sent", d.UpstreamModel)
		}
	}

	// A caller's input estimate stands for every target, with the bound it names.
	decisions = simulateStrict(t, snapshot, simulationInput{Request: json.RawMessage(body), EstimatedInputTokens: &input, MaxOutputTokens: &output})
	for _, d := range decisions {
		if d.EstimatedInputTokens == nil || *d.EstimatedInputTokens != 10 || d.EstimateProvenance != nil {
			t.Errorf("%s: estimated input %v from %v, want the caller's 10", d.UpstreamModel, deref(d.EstimatedInputTokens), deref(d.EstimateProvenance))
		}
		if d.RequestedOutputTokens == nil || *d.RequestedOutputTokens != 100 || !d.Eligible {
			t.Errorf("%s: requested output %v, eligible %v (%v), want the caller's 100 to fit beside 10", d.UpstreamModel, deref(d.RequestedOutputTokens), d.Eligible, deref(d.Reason))
		}
	}
}

// fixedSimulation simulates without a selected key and with every credential
// eligible.
func fixedSimulation(snapshot *runtime.Snapshot, input simulationInput, demand *runtime.TokenDemand) *simulation {
	m := (&Server{}).newSimulation(context.Background(), snapshot, input, nil, demand)
	m.key = func(runtime.Route) (inspectionKeyContext, error) { return inspectionKeyContext{}, nil }
	m.eligibility = func(runtime.Route) (func(string) runtime.Eligibility, error) {
		return func(string) runtime.Eligibility { return runtime.Eligible }, nil
	}
	return m
}
