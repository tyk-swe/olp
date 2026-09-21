package runtime

import (
	"testing"
)

func decisionsByProvider(plan Plan, ids []string) map[string]Decision {
	out := map[string]Decision{}
	for _, d := range plan.Decisions {
		out[d.ProviderID] = d
	}
	return out
}

func TestCapacityExcludesOnlyDeclaredLimits(t *testing.T) {
	s, slug, ids := planningFixture()
	withMetadata(&s, ids[0], "wire-model", ModelMetadata{ContextLength: ptr(int64(100)), MaxOutputTokens: ptr(int64(50))})
	withMetadata(&s, ids[1], "wire-model", ModelMetadata{ContextLength: ptr(int64(1000))})

	plan, err := PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{
		TokenDemand: &TokenDemand{EstimatedInputTokens: 200},
	})
	if err != nil {
		t.Fatal(err)
	}
	decisions := decisionsByProvider(plan, ids)
	if decisions[ids[0]].Eligible || decisions[ids[0]].Reason == nil || *decisions[ids[0]].Reason != "context_length_exceeded" {
		t.Fatalf("declared context must exclude: %+v", decisions[ids[0]])
	}
	for _, id := range ids[1:] {
		if !decisions[id].Eligible {
			t.Fatalf("target %s wrongly excluded: %+v", id, decisions[id])
		}
	}

	plan, err = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{
		TokenDemand: &TokenDemand{EstimatedInputTokens: 90, MaxOutputTokens: ptr(int64(950))},
	})
	if err != nil {
		t.Fatal(err)
	}
	decisions = decisionsByProvider(plan, ids)
	if decisions[ids[1]].Eligible || *decisions[ids[1]].Reason != "context_length_exceeded" {
		t.Fatalf("input+output must exclude: %+v", decisions[ids[1]])
	}
	if !decisions[ids[2]].Eligible {
		t.Fatalf("unknown metadata must stay eligible: %+v", decisions[ids[2]])
	}

	plan, err = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{
		TokenDemand: &TokenDemand{EstimatedInputTokens: 10, MaxOutputTokens: ptr(int64(60))},
	})
	if err != nil {
		t.Fatal(err)
	}
	decisions = decisionsByProvider(plan, ids)
	if decisions[ids[0]].Eligible || *decisions[ids[0]].Reason != "max_output_tokens_exceeded" {
		t.Fatalf("declared max output must exclude: %+v", decisions[ids[0]])
	}
	if !decisions[ids[1]].Eligible || !decisions[ids[2]].Eligible {
		t.Fatalf("targets without a declared bound must stay eligible: %+v", decisions)
	}
}

func TestDecisionsExposeDemandAndObservedLimits(t *testing.T) {
	s, slug, ids := planningFixture()
	withMetadata(&s, ids[0], "wire-model", ModelMetadata{ContextLength: ptr(int64(100)), MaxOutputTokens: ptr(int64(50))})
	plan, err := PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{
		TokenDemand: &TokenDemand{EstimatedInputTokens: 10, MaxOutputTokens: ptr(int64(20))},
	})
	if err != nil {
		t.Fatal(err)
	}
	known := decisionsByProvider(plan, ids)[ids[0]]
	if known.EstimatedInputTokens == nil || *known.EstimatedInputTokens != 10 || known.RequestedOutputTokens == nil || *known.RequestedOutputTokens != 20 {
		t.Fatalf("demand not recorded: %+v", known)
	}
	if known.ContextLength == nil || *known.ContextLength != 100 || known.MaxOutputTokens == nil || *known.MaxOutputTokens != 50 {
		t.Fatalf("observed limits not recorded: %+v", known)
	}
	unknown := decisionsByProvider(plan, ids)[ids[2]]
	if unknown.ContextLength != nil || unknown.MaxOutputTokens != nil {
		t.Fatalf("undeclared facts must stay nil: %+v", unknown)
	}
}
