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

// TestDemandIsWeighedPerTarget lets a request count differently on each model:
// the same prompt fits one target's window and not another's, and each
// decision records the demand its own target was weighed by.
func TestDemandIsWeighedPerTarget(t *testing.T) {
	s, slug, ids := planningFixture()
	withMetadata(&s, ids[0], "wire-model", ModelMetadata{ContextLength: ptr(int64(100))})
	withMetadata(&s, ids[1], "wire-model", ModelMetadata{ContextLength: ptr(int64(100))})

	weighed := map[string]int{}
	plan, err := PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{
		// A per-target demand stands in for the request-wide one.
		TokenDemand: &TokenDemand{EstimatedInputTokens: 1},
		Demand: func(p Provider, _ Target) *TokenDemand {
			weighed[p.ID]++
			if p.ID == ids[0] {
				return &TokenDemand{EstimatedInputTokens: 150}
			}
			return &TokenDemand{EstimatedInputTokens: 40, MaxOutputTokens: ptr(int64(10))}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	decisions := decisionsByProvider(plan, ids)
	if d := decisions[ids[0]]; d.Eligible || d.Reason == nil || *d.Reason != "context_length_exceeded" || d.EstimatedInputTokens == nil || *d.EstimatedInputTokens != 150 {
		t.Fatalf("the target the demand does not fit: %+v", d)
	}
	if d := decisions[ids[1]]; !d.Eligible || d.EstimatedInputTokens == nil || *d.EstimatedInputTokens != 40 || d.RequestedOutputTokens == nil || *d.RequestedOutputTokens != 10 {
		t.Fatalf("the target it fits: %+v", d)
	}
	if weighed[ids[0]] != 1 || weighed[ids[1]] != 1 {
		t.Fatalf("demand asked %v times, want once for each target", weighed)
	}
}
