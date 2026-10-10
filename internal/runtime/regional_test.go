package runtime

import (
	"encoding/json"
	"testing"
	"time"
)

func TestLocalProviderPreferencePreservesPriorityAndHardRegionConstraints(t *testing.T) {
	s, slug, ids := planningFixture()
	now := time.Now()
	for i, id := range ids {
		provider := s.Providers[id]
		region := "east"
		if i == 1 {
			region = "west"
		}
		provider.CloudRegion = region
		metadata, _ := json.Marshal(ModelMetadata{Region: &region, Source: ptr("contract"), ObservedAt: &now})
		provider.Models = map[string]json.RawMessage{"wire-model": metadata}
		s.Providers[id] = provider
	}
	plan, err := PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{Region: "west", Now: now})
	if err != nil || len(plan.Attempts) != 3 || plan.Attempts[0].ProviderID != ids[1] {
		t.Fatalf("local preference: %+v %v", plan, err)
	}
	route := s.Routes[slug]
	route.Targets[1].Priority = 1
	route.Targets[2].Priority = 1
	s.Routes[slug] = route
	plan, err = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{Region: "west", Now: now})
	if err != nil || plan.Attempts[0].ProviderID != ids[0] {
		t.Fatalf("local preference crossed priority tiers: %+v %v", plan, err)
	}
	s.InstallationPolicy = &Policy{Constraints: Preferences{Regions: []string{"east"}}}
	plan, err = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{Region: "west", Now: now})
	if err != nil || len(plan.Attempts) != 2 {
		t.Fatalf("hard region constraints: %+v %v", plan, err)
	}
	for _, attempt := range plan.Attempts {
		if attempt.ProviderID == ids[1] {
			t.Fatal("locality admitted a forbidden region")
		}
	}
}
