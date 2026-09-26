package runtime

import (
	"reflect"

	"github.com/tyk-swe/olp/internal/interaction"
)

// PinnedCurrent reuses templates from an already-validated installed release
// only when a retained resource names that exact published provider, route,
// target and credential slot. Historical or drifted revisions still require
// the resource resolver's full reconstruction and validation path.
func (s *Snapshot) PinnedCurrent(route Route, provider Provider, target Target, slot Slot, keyID, surface, mode string) (*Snapshot, Slot, bool) {
	if s == nil {
		return nil, Slot{}, false
	}
	currentRoute, ok := s.Routes[route.Slug]
	if !ok || currentRoute.ID != route.ID || currentRoute.RevisionID != route.RevisionID {
		return nil, Slot{}, false
	}
	currentProvider, ok := s.Providers[provider.ID]
	if !ok || currentProvider.RevisionID != provider.RevisionID || currentProvider.Kind != provider.Kind || !currentProvider.Enabled ||
		!currentProvider.Supports(target.ProviderModel, "generation", surface, mode) {
		return nil, Slot{}, false
	}
	matchedTarget := false
	for _, candidate := range currentRoute.Targets {
		if candidate.ID == target.ID && candidate == target && candidate.ProviderID == provider.ID && candidate.ProviderModel == target.ProviderModel {
			matchedTarget = true
			break
		}
	}
	if !matchedTarget {
		return nil, Slot{}, false
	}
	var selected Slot
	matchedSlot := false
	for _, candidate := range currentProvider.Slots {
		if candidate.ID == slot.ID && reflect.DeepEqual(candidate, slot) && candidate.Allows(target.ProviderModel, route.Slug, keyID) {
			selected, matchedSlot = candidate, true
			break
		}
	}
	if !matchedSlot {
		return nil, Slot{}, false
	}
	if currentRoute.Fidelity.Strict() {
		if s.interactions[currentRoute.Slug][target.ID] == nil {
			return nil, Slot{}, false
		}
	}
	currentRoute.Targets = []Target{target}
	currentProvider.Slots = []Slot{selected}
	retained := &Snapshot{
		Generation: s.Generation, Providers: map[string]Provider{provider.ID: currentProvider},
		Routes: map[string]Route{route.Slug: currentRoute}, InstallationPolicy: s.InstallationPolicy,
		KeyPolicies: s.KeyPolicies, interactions: map[string]map[string]*interaction.Template{},
	}
	if templates := s.interactions[route.Slug]; templates != nil {
		retained.interactions[route.Slug] = map[string]*interaction.Template{target.ID: templates[target.ID]}
	}
	return retained, selected, true
}
