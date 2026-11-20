package routes

import (
	"errors"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/routing"
)

type fixtureResult struct {
	Name           string      `json:"name"`
	Published      fixturePlan `json:"published"`
	Draft          fixturePlan `json:"draft"`
	Changes        []string    `json:"changes"`
	ExpectEligible *bool       `json:"expect_eligible"`
	Passed         bool        `json:"passed"`
}

type fixturePlan struct {
	Winner    *fixtureTarget         `json:"winner"`
	Reason    *string                `json:"reason"`
	Targets   []fixtureDecision      `json:"targets"`
	Selectors []fixtureSelector      `json:"selectors"`
	Fallbacks []routing.FallbackStep `json:"fallbacks"`
}

type fixtureTarget struct {
	Route        string `json:"route"`
	ProviderID   string `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	Model        string `json:"model"`
}

type fixtureDecision struct {
	fixtureTarget
	Eligible              bool                  `json:"eligible"`
	Shadow                bool                  `json:"shadow"`
	Reason                *string               `json:"reason"`
	ContextWindow         *contextWindowPreview `json:"context_window,omitempty"`
	ContextLength         *int64                `json:"context_length"`
	EstimatedInputTokens  *int64                `json:"estimated_input_tokens"`
	RequestedOutputTokens *int64                `json:"requested_output_tokens"`
	EstimateProvenance    *string               `json:"estimate_provenance"`
}

type fixtureSelector struct {
	Route   string `json:"route"`
	ID      string `json:"id"`
	Matched bool   `json:"matched"`
	Outcome string `json:"outcome"`
}

func (s *Server) fixturePlan(shared simulation, snapshot *routing.Snapshot, input simulationInput, slug string) (fixturePlan, error) {
	m := shared
	m.snapshot, m.input = snapshot, input
	plan, err := s.explain(&m, slug)
	if err != nil {
		var refusal *routing.SelectionError
		if errors.As(err, &refusal) {
			return fixturePlan{Reason: &refusal.Code, Targets: []fixtureDecision{}, Selectors: []fixtureSelector{}, Fallbacks: []routing.FallbackStep{}}, nil
		}
		return fixturePlan{}, err
	}
	return summarizeFixturePlan(plan, snapshot), nil
}

func summarizeFixturePlan(plan explanation, snapshot *routing.Snapshot) fixturePlan {
	out := fixturePlan{Targets: []fixtureDecision{}, Selectors: []fixtureSelector{}, Fallbacks: plan.Fallbacks}
	for _, leg := range plan.Legs {
		for _, selector := range leg.Selectors {
			out.Selectors = append(out.Selectors, fixtureSelector{leg.Route, selector.ID, selector.Matched, selector.Outcome})
		}
		for _, decision := range leg.Decisions {
			target := fixtureTarget{Route: leg.Route, ProviderID: decision.ProviderID, ProviderName: snapshot.Providers[decision.ProviderID].Name, Model: decision.UpstreamModel}
			if decision.Attempt != nil && *decision.Attempt == 1 {
				out.Winner = &target
			}
			out.Targets = append(out.Targets, fixtureDecision{
				fixtureTarget: target, Eligible: decision.Eligible, Shadow: decision.Shadow, Reason: decision.Reason,
				ContextWindow: decision.ContextWindow, ContextLength: decision.ContextLength,
				EstimatedInputTokens: decision.EstimatedInputTokens, RequestedOutputTokens: decision.RequestedOutputTokens,
				EstimateProvenance: decision.EstimateProvenance,
			})
		}
	}
	if out.Winner == nil {
		out.Reason = new(routing.NoEligibleTargets)
	}
	return out
}

func diffFixturePlans(before, after fixturePlan) []string {
	changes := []string{}
	if !sameFixtureValue(before.Winner, after.Winner) {
		changes = append(changes, "winner")
	}
	if (before.Winner == nil) != (after.Winner == nil) ||
		!maps.Equal(fixtureFacts(before.Targets, eligibilityFact), fixtureFacts(after.Targets, eligibilityFact)) {
		changes = append(changes, "eligibility")
	}
	if !slices.Equal(effectiveFixtureSelectors(before), effectiveFixtureSelectors(after)) {
		changes = append(changes, "selector")
	}
	if !sameFixtureValue(before.Reason, after.Reason) ||
		!slices.EqualFunc(before.Fallbacks, after.Fallbacks, sameFixtureFallback) ||
		!maps.Equal(fixtureFacts(before.Targets, reasonFact), fixtureFacts(after.Targets, reasonFact)) {
		changes = append(changes, "reason")
	}
	return changes
}

func effectiveFixtureSelectors(plan fixturePlan) []fixtureSelector {
	return slices.Collect(func(yield func(fixtureSelector) bool) {
		for _, selector := range plan.Selectors {
			if selector.Outcome != "not_matched" && !yield(selector) {
				return
			}
		}
	})
}

func sameFixtureValue[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func sameFixtureFallback(a, b routing.FallbackStep) bool {
	return a.From == b.From && a.Route == b.Route && a.Outcome == b.Outcome && slices.Equal(a.Conditions, b.Conditions)
}

type fixtureFact[T comparable] struct {
	fixtureTarget
	Value T
}

func fixtureFacts[T comparable](targets []fixtureDecision, fact func(fixtureDecision) (T, bool)) map[fixtureFact[T]]bool {
	out := make(map[fixtureFact[T]]bool, len(targets))
	for _, target := range targets {
		if value, ok := fact(target); ok {
			out[fixtureFact[T]{target.fixtureTarget, value}] = true
		}
	}
	return out
}

func eligibilityFact(target fixtureDecision) (bool, bool) {
	return target.Eligible, !target.Shadow
}

func reasonFact(target fixtureDecision) (string, bool) {
	if target.Reason == nil || target.Shadow {
		return "", false
	}
	return *target.Reason, true
}
