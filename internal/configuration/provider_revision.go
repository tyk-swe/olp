package configuration

import (
	"encoding/json"
	"slices"

	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routing"
)

func portableProviderRevision(base ProviderEntry, configuration, models, slots []byte) (ProviderEntry, error) {
	var cfg providers.Configuration
	if err := json.Unmarshal(configuration, &cfg); err != nil {
		return ProviderEntry{}, err
	}
	var inventory []routing.RevisionModel
	if err := json.Unmarshal(models, &inventory); err != nil {
		return ProviderEntry{}, err
	}
	var credentials []routing.RevisionSlot
	if err := json.Unmarshal(slots, &credentials); err != nil {
		return ProviderEntry{}, err
	}
	entry := ProviderEntry{Name: base.Name, Project: base.Project, Configuration: cfg, Models: mapCode(inventory, func(m routing.RevisionModel) ModelEntry {
		return ModelEntry{UpstreamModel: m.UpstreamModel, DisplayName: m.DisplayName, Enabled: true, Capabilities: mapCode(m.Capabilities, func(c routing.RevisionCapability) CapabilityEntry {
			return CapabilityEntry{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode}
		})}
	}), Slots: mapCode(slicesIndex(credentials), func(index indexedCode[routing.RevisionSlot]) SlotEntry {
		slot := index.Value
		ref := func() *string {
			if slot.CredentialID == nil {
				return nil
			}
			return new(CredentialRef(base.Name, slot.Name))
		}()
		return SlotEntry{Name: slot.Name, IsDefault: slot.Default, Position: index.Index, Enabled: slot.Enabled, Priority: slot.Priority, Weight: slot.Weight, Restrictions: Restrictions{AllowedAPIKeys: []string{}, AllowedModels: orEmpty(slot.AllowedModels), AllowedRoutes: orEmpty(slot.AllowedRoutes)}, Limits: providers.Limits{MaxConcurrency: slot.MaxConcurrency, RequestsPerMinute: slot.RequestsPerMinute, TokensPerMinute: slot.TokensPerMinute, Supply: slot.Supply}, CredentialRef: ref}
	})}
	portableNetwork(&entry)
	referenceGrants(&entry)
	return entry, nil
}

type indexedCode[T any] struct {
	Index int
	Value T
}

func slicesIndex[T any](values []T) []indexedCode[T] {
	return slices.Collect(func(yield func(indexedCode[T]) bool) {
		for index, value := range values {
			if !yield(indexedCode[T]{Index: index, Value: value}) {
				return
			}
		}
	})
}
