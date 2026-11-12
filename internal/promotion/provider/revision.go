package provider

import (
	"encoding/json"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/routing"
)

type revision struct {
	name          string
	project       *string
	configuration []byte
	models        []byte
	slots         []byte
}

func FromRevision(name string, project *string, configuration, models, slots []byte) (Entry, error) {
	return fromRevision(revision{name: name, project: project, configuration: configuration, models: models, slots: slots}, nil, nil)
}

func publishedEntry(row providers.ProviderSnapshot, slots []providers.SlotSnapshot, imported *Billing) (*Entry, error) {
	if row.RevisionConfiguration == nil {
		return nil, nil
	}
	draft := maps.Collect(func(yield func(string, int) bool) {
		for _, slot := range slots {
			if !yield(slot.Name, slot.Position) {
				return
			}
		}
	})
	entry, err := fromRevision(revision{name: row.Name, project: row.Project, configuration: row.RevisionConfiguration, models: row.RevisionModels, slots: row.RevisionSlots}, draft, imported)
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

func fromRevision(stored revision, draft map[string]int, imported *Billing) (Entry, error) {
	var configuration connection.Configuration
	if err := json.Unmarshal(stored.configuration, &configuration); err != nil {
		return Entry{}, err
	}
	var models []routing.RevisionModel
	if err := json.Unmarshal(stored.models, &models); err != nil {
		return Entry{}, err
	}
	var slots []routing.RevisionSlot
	if err := json.Unmarshal(stored.slots, &slots); err != nil {
		return Entry{}, err
	}
	positions := revisionPositions(slots, draft)
	return Entry{
		Name:          stored.name,
		Project:       stored.project,
		Configuration: configuration.WithNetworkCredential(""),
		Models: promotion.Map(models, func(model routing.RevisionModel) providers.DraftModel {
			return providers.DraftModel{UpstreamModel: model.UpstreamModel, DisplayName: model.DisplayName, Enabled: true, Capabilities: promotion.Map(model.Capabilities, func(capability routing.RevisionCapability) connection.Capability {
				return connection.Capability{Operation: capability.Operation, Surface: capability.Surface, Mode: capability.Mode}
			})}
		}),
		Slots: promotion.MapIndex(slots, func(i int, slot routing.RevisionSlot) Slot {
			return Slot{
				Name:          slot.Name,
				Default:       slot.Default,
				Position:      positions[i],
				Enabled:       slot.Enabled,
				Priority:      slot.Priority,
				Weight:        slot.Weight,
				AllowedModels: promotion.NonNil(slot.AllowedModels),
				AllowedRoutes: promotion.NonNil(slot.AllowedRoutes),
				Limits:        connection.Limits{MaxConcurrency: slot.MaxConcurrency, RequestsPerMinute: slot.RequestsPerMinute, TokensPerMinute: slot.TokensPerMinute, Supply: slot.Supply},
				Credential:    storedCredential(configuration.Grant(), slot.CredentialID != nil),
			}
		}),
		Network: storedNetwork(configuration),
		Billing: imported,
	}, nil
}

func revisionPositions(slots []routing.RevisionSlot, draft map[string]int) []int {
	indices := promotion.MapIndex(slots, func(i int, _ routing.RevisionSlot) int { return i })
	if slices.ContainsFunc(slots, func(slot routing.RevisionSlot) bool { _, ok := draft[slot.Name]; return !ok }) {
		return indices
	}
	positions := promotion.Map(slots, func(slot routing.RevisionSlot) int { return draft[slot.Name] })
	if _, repeated := promotion.FirstRepeat(positions, func(position int) int { return position }); repeated {
		return indices
	}
	return positions
}
