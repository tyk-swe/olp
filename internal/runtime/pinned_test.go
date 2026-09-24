package runtime

import (
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/interaction"
)

func TestPinnedCurrentReusesOnlyTheExactInstalledServingRevision(t *testing.T) {
	snapshot, slug, ids := planningFixture()
	route := snapshot.Routes[slug]
	route.ID, route.RevisionID = uuid.NewString(), uuid.NewString()
	route.Fidelity = &RouteFidelity{Mode: FidelityStrict}
	snapshot.Routes[slug] = route
	provider := snapshot.Providers[ids[0]]
	slot := Slot{ID: uuid.NewString(), Enabled: true, Weight: 1}
	provider.Slots = []Slot{slot}
	snapshot.Providers[provider.ID] = provider
	target := route.Targets[0]
	template := &interaction.Template{}
	snapshot.interactions = map[string]map[string]*interaction.Template{slug: {target.ID: template}}

	pinned, selected, ok := snapshot.PinnedCurrent(route, provider, target, slot, "key", "openai", "unary")
	if !ok || selected.ID != slot.ID || len(pinned.Routes[slug].Targets) != 1 || len(pinned.Providers[provider.ID].Slots) != 1 || pinned.interactions[slug][target.ID] != template {
		t.Fatal("exact current revision did not retain its compiled target")
	}
	if len(snapshot.Routes[slug].Targets) != 3 || len(snapshot.Providers[provider.ID].Slots) != 1 {
		t.Fatal("pinning mutated the installed release")
	}
	changed := route
	changed.RevisionID = uuid.NewString()
	if _, _, ok := snapshot.PinnedCurrent(changed, provider, target, slot, "key", "openai", "unary"); ok {
		t.Fatal("historical route revision reused a current plan")
	}
	changed = route
	provider.RevisionID = uuid.NewString()
	if _, _, ok := snapshot.PinnedCurrent(changed, provider, target, slot, "key", "openai", "unary"); ok {
		t.Fatal("historical provider revision reused a current plan")
	}
	provider = snapshot.Providers[ids[0]]
	target.ProviderModel = "different-model"
	if _, _, ok := snapshot.PinnedCurrent(route, provider, target, slot, "key", "openai", "unary"); ok {
		t.Fatal("changed target model reused a current plan")
	}
	target = route.Targets[0]
	slot.Enabled = false
	if _, _, ok := snapshot.PinnedCurrent(route, provider, target, slot, "key", "openai", "unary"); ok {
		t.Fatal("disabled or drifted credential slot reused a current plan")
	}
	slot = provider.Slots[0]
	delete(snapshot.interactions[slug], target.ID)
	if _, _, ok := snapshot.PinnedCurrent(route, provider, target, slot, "key", "openai", "unary"); ok {
		t.Fatal("strict route without its compiled template reused a current plan")
	}
}
