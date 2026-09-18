package runtime

import (
	"slices"
	"testing"

	"github.com/google/uuid"
)

func TestPlanPreservesTargetOrderWhenScoresTie(t *testing.T) {
	for _, strategy := range []string{"weighted", "price", "latency", "throughput"} {
		t.Run(strategy, func(t *testing.T) {
			snapshot, slug, _ := planningFixture()
			route := snapshot.Routes[slug]
			// Shared routing inputs force identical rendezvous scores.
			routingID := uuid.NewString()
			var want []string
			for i := range route.Targets {
				route.Targets[i].RoutingID = routingID
				route.Targets[i].Weight = 1
				want = append(want, route.Targets[i].ID)
			}
			snapshot.Routes[slug] = route
			plan, err := PlanRequest(&snapshot, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{Preferences: &Preferences{Strategy: new(strategy)}})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, attempt := range plan.Attempts {
				got = append(got, attempt.TargetID)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("tied target order = %v, want %v", got, want)
			}
		})
	}
}

func TestSelectSlotsPreservesPriorityAndTiedOrder(t *testing.T) {
	provider := Provider{Kind: "openai_compatible", AuthMode: "none"}
	// Reuse the score inputs to exercise a tie independently of the hash.
	id := uuid.NewString()
	for _, name := range []string{"first", "second", "third"} {
		provider.Slots = append(provider.Slots, Slot{ID: id, Name: name, Enabled: true, Weight: 1})
	}
	provider.Slots = append(provider.Slots, Slot{ID: id, Name: "priority", Enabled: true, Weight: 1, Priority: -1})
	route := Route{RoutingID: uuid.NewString(), Slug: "route"}
	slots := SelectSlots(provider, "model", route, "", "generation", "openai", "unary", []byte("seed"))
	var names []string
	for _, slot := range slots {
		names = append(names, slot.Name)
	}
	want := []string{"priority", "first", "second", "third"}
	if !slices.Equal(names, want) {
		t.Fatalf("slot order = %v, want %v", names, want)
	}
}
