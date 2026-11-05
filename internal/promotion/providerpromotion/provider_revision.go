package providerpromotion

import (
	"encoding/json"
	"github.com/tyk-swe/olp/internal/promotion"
	"slices"

	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routing"
)

func PortableProviderRevision(base promotion.ProviderEntry, configuration, models, slots []byte) (promotion.ProviderEntry, error) {
	var cfg providers.Configuration
	if err := json.Unmarshal(configuration, &cfg); err != nil {
		return promotion.ProviderEntry{}, err
	}
	var inventory []routing.RevisionModel
	if err := json.Unmarshal(models, &inventory); err != nil {
		return promotion.ProviderEntry{}, err
	}
	var credentials []routing.RevisionSlot
	if err := json.Unmarshal(slots, &credentials); err != nil {
		return promotion.ProviderEntry{}, err
	}
	entry := promotion.ProviderEntry{Name: base.Name, Project: base.Project, Configuration: cfg, Models: promotion.MapCode(inventory, func(m routing.RevisionModel) promotion.ModelEntry {
		return promotion.ModelEntry{UpstreamModel: m.UpstreamModel, DisplayName: m.DisplayName, Enabled: true, Capabilities: promotion.MapCode(m.Capabilities, func(c routing.RevisionCapability) promotion.CapabilityEntry {
			return promotion.CapabilityEntry{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode}
		})}
	}), Slots: promotion.MapCode(slicesIndex(credentials), func(index indexedCode[routing.RevisionSlot]) promotion.SlotEntry {
		slot := index.Value
		ref := func() *string {
			if slot.CredentialID == nil {
				return nil
			}
			return new(promotion.CredentialRef(base.Name, slot.Name))
		}()
		return promotion.SlotEntry{Name: slot.Name, IsDefault: slot.Default, Position: index.Index, Enabled: slot.Enabled, Priority: slot.Priority, Weight: slot.Weight, Restrictions: promotion.Restrictions{AllowedAPIKeys: []string{}, AllowedModels: promotion.OrEmpty(slot.AllowedModels), AllowedRoutes: promotion.OrEmpty(slot.AllowedRoutes)}, Limits: providers.Limits{MaxConcurrency: slot.MaxConcurrency, RequestsPerMinute: slot.RequestsPerMinute, TokensPerMinute: slot.TokensPerMinute, Supply: slot.Supply}, CredentialRef: ref}
	})}
	PortableNetwork(&entry)
	ReferenceGrants(&entry)
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
