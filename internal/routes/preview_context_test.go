package routes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestPreviewExplainsWhatConsumesContext(t *testing.T) {
	const private = "private-tool-description"
	snapshot, route := contextPreviewFixture()
	request := func(description string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Search the records."}],
			"tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object","description":%q}}}],"max_completion_tokens":64,"n":3}`,
			route.Slug, description))
	}
	large := explainContextFixture(t, snapshot, route, wire.Chat, request(strings.Repeat(private+" ", 512)), nil)
	small := explainContextFixture(t, snapshot, route, wire.Chat, request("Search filter."), nil)
	for _, leg := range []simulatedLeg{large, small} {
		for _, decision := range leg.Decisions {
			window := decision.ContextWindow
			if window == nil || window.InputBreakdown == nil || decision.EstimatedInputTokens == nil || decision.EstimateProvenance == nil ||
				decision.RequestedOutputTokens == nil || *decision.RequestedOutputTokens != 64 {
				t.Fatalf("missing planning estimates: %+v", decision)
			}
			b := window.InputBreakdown
			input := *decision.EstimatedInputTokens
			if sum := b.Instructions + b.Conversation + b.ToolSchemas + b.ToolCallsResults + b.Media + b.Unclassified; sum != input {
				t.Fatalf("input categories sum to %d, expected %d", sum, input)
			}
			if decision.ContextLength == nil {
				if window.RemainingTokens != nil {
					t.Fatalf("unknown model limit produced headroom: %+v", window)
				}
			} else if window.RemainingTokens == nil || *window.RemainingTokens != *decision.ContextLength-input-64 {
				t.Fatalf("headroom does not use one completion's output bound: %+v", window)
			}
		}
	}
	before, after := decisionWithContextLimit(t, large), decisionWithContextLimit(t, small)
	if before.ContextWindow.InputBreakdown.ToolSchemas <= *before.EstimatedInputTokens/2 ||
		*before.ContextWindow.RemainingTokens >= 0 || *after.ContextWindow.RemainingTokens <= 0 ||
		*after.ContextWindow.RemainingTokens-*before.ContextWindow.RemainingTokens != *before.EstimatedInputTokens-*after.EstimatedInputTokens {
		t.Fatalf("trimming the dominant schemas did not recover headroom: %+v vs %+v", before.ContextWindow, after.ContextWindow)
	}
	body, err := json.Marshal(large)
	if err != nil || bytes.Contains(body, []byte(private)) {
		t.Fatal("the simulation must not echo tool schema contents")
	}
}

func TestPreviewDoesNotClaimUnmeasuredContext(t *testing.T) {
	snapshot, route := contextPreviewFixture()
	retained := json.RawMessage(`{"model":"review","input":"Continue.","previous_response_id":"resp_example"}`)
	decision := decisionWithContextLimit(t, explainContextFixture(t, snapshot, route, wire.Responses, retained, nil))
	if window := decision.ContextWindow; window == nil || !window.RetainedContextUnknown || window.InputBreakdown == nil || window.RemainingTokens != nil {
		t.Fatalf("a retained request should show inline tokens but unknown headroom: %+v", window)
	}
	inline := json.RawMessage(`{"model":"review","input":"Continue.","previous_response_id":null}`)
	fixed := decisionWithContextLimit(t, explainContextFixture(t, snapshot, route, wire.Responses, inline, new(int64(0))))
	if window := fixed.ContextWindow; window == nil || window.RetainedContextUnknown || window.InputBreakdown != nil ||
		window.RemainingTokens == nil || *window.RemainingTokens != *fixed.ContextLength || fixed.EstimateProvenance != nil {
		t.Fatalf("a caller estimate has no measured breakdown and no retained context: %+v", window)
	}
	tupleOnly := decisionWithContextLimit(t, explainContextFixture(t, snapshot, route, wire.Chat, nil, nil))
	if tupleOnly.ContextWindow != nil {
		t.Fatal("tuple-only preview unexpectedly has a generation breakdown")
	}
}

func contextPreviewFixture() (*routing.Snapshot, routing.Route) {
	id, known, unknown := uuid.NewString(), uuid.NewString(), uuid.NewString()
	capabilities := []routing.Capability{{Model: "fixture-model", Operation: "generation", Surface: "openai", Mode: "unary"}}
	target := func(provider string) routing.Target {
		targetID := uuid.NewString()
		return routing.Target{ID: targetID, RoutingID: targetID, ProviderID: provider, ProviderModel: "fixture-model", Weight: 1, Timeout: 1000}
	}
	route := routing.Route{ID: id, RoutingID: id, Slug: "review", MaxAttempts: 2, OverallTimeout: 3000, Operations: []string{"generation"},
		Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed}, Targets: []routing.Target{target(known), target(unknown)}}
	return &routing.Snapshot{
		Routes: map[string]routing.Route{route.Slug: route},
		Providers: map[string]routing.Provider{
			known: {ID: known, Kind: "openai", Enabled: true, Capabilities: capabilities,
				Models: map[string]json.RawMessage{"fixture-model": json.RawMessage(`{"context_length":2048}`)}},
			unknown: {ID: unknown, Kind: "openai", Enabled: true, Capabilities: capabilities},
		},
	}, route
}

func explainContextFixture(t *testing.T, snapshot *routing.Snapshot, route routing.Route, family wire.Format, request json.RawMessage, fixed *int64) simulatedLeg {
	t.Helper()
	s := Server{Access: &access.Server{}}
	demand, err := tokenDemand(fixed, nil)
	if err != nil {
		t.Fatal(err)
	}
	input := simulationInput{Operation: "generation", Surface: "openai", Mode: "unary", Dialect: wire.Descriptor(family, false).Dialect.ID, Request: request, EstimatedInputTokens: fixed}
	m := s.newSimulation(t.Context(), snapshot, input, nil, demand)
	m.key = func(routing.Route) (inspectionKeyContext, error) { return inspectionKeyContext{}, nil }
	m.eligibility = func(routing.Route) (func(string) routing.Eligibility, error) { return nil, nil }
	result, err := s.explain(m, route.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Legs) != 1 || len(result.Legs[0].Decisions) != len(route.Targets) {
		t.Fatalf("unexpected simulation result: %+v", result)
	}
	return result.Legs[0]
}

func decisionWithContextLimit(t *testing.T, leg simulatedLeg) inspectedDecision {
	t.Helper()
	index := slices.IndexFunc(leg.Decisions, func(decision inspectedDecision) bool { return decision.ContextLength != nil })
	if index < 0 {
		t.Fatal("fixture produced no decision with a context limit")
	}
	return leg.Decisions[index]
}
