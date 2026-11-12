package provider

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/billing/imports"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

type destination struct {
	id        string
	kind      string
	project   *string
	network   *string
	billing   *imports.Config
	slots     map[string]storedSlot
	draft     Entry
	published *Entry
}

type storedSlot struct {
	id             string
	credentialID   *string
	pluginDigest   string
	profileID      string
	allowedAPIKeys []string
}

func Read(ctx context.Context, q access.Queryer) ([]Entry, error) {
	destinations, err := readDestinations(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.Map(slices.Collect(maps.Values(destinations)), func(existing destination) Entry {
		if existing.published != nil {
			return *existing.published
		}
		return existing.draft
	}), nil
}

func readDestinations(ctx context.Context, q access.Queryer) (map[string]destination, error) {
	rows, err := providers.ReadProviderSnapshots(ctx, q)
	if err != nil {
		return nil, err
	}
	models, err := providers.ReadModelSnapshots(ctx, q)
	if err != nil {
		return nil, err
	}
	slots, err := providers.ReadSlotSnapshots(ctx, q)
	if err != nil {
		return nil, err
	}
	destinations, err := promotion.TryMap(rows, func(row providers.ProviderSnapshot) (destination, error) {
		return readDestination(ctx, q, row, promotion.Filter(models, func(model providers.ModelSnapshot) bool { return model.ProviderID == row.ID }), promotion.Filter(slots, func(slot providers.SlotSnapshot) bool { return slot.ProviderID == row.ID }))
	})
	if err != nil {
		return nil, err
	}
	return maps.Collect(func(yield func(string, destination) bool) {
		for i, existing := range destinations {
			if !yield(strings.ToLower(rows[i].Name), existing) {
				return
			}
		}
	}), nil
}

func readDestination(ctx context.Context, q access.Queryer, row providers.ProviderSnapshot, models []providers.ModelSnapshot, slots []providers.SlotSnapshot) (destination, error) {
	imported, err := readImport(ctx, q, row)
	if err != nil {
		return destination{}, err
	}
	var configuration connection.Configuration
	if err := json.Unmarshal(row.Configuration, &configuration); err != nil {
		return destination{}, err
	}
	stored, err := promotion.TryMap(slots, func(slot providers.SlotSnapshot) (storedPair, error) {
		return parseSlotRow(slot, configuration.Grant())
	})
	if err != nil {
		return destination{}, err
	}
	draft, err := draftEntry(row, configuration, models, stored, storedBilling(imported))
	if err != nil {
		return destination{}, err
	}
	published, err := publishedEntry(row, slots, storedBilling(imported))
	if err != nil {
		return destination{}, err
	}
	return destination{
		id:        row.ID,
		kind:      row.Kind,
		project:   row.Project,
		network:   row.Network,
		billing:   imported,
		slots:     storedSlots(stored),
		draft:     draft,
		published: published,
	}, nil
}

func readImport(ctx context.Context, q access.Queryer, row providers.ProviderSnapshot) (*imports.Config, error) {
	if !row.Imported {
		return nil, nil
	}
	config, found, err := imports.Read(ctx, q, row.ID)
	if err != nil || !found {
		return nil, err
	}
	return &config, nil
}

func draftEntry(row providers.ProviderSnapshot, configuration connection.Configuration, models []providers.ModelSnapshot, slots []storedPair, imported *Billing) (Entry, error) {
	draftModels, err := promotion.TryMap(models, func(model providers.ModelSnapshot) (providers.DraftModel, error) {
		var capabilities []connection.Capability
		if err := json.Unmarshal(model.Capabilities, &capabilities); err != nil {
			return providers.DraftModel{}, err
		}
		return providers.DraftModel{UpstreamModel: model.UpstreamModel, DisplayName: model.DisplayName, Enabled: model.Enabled, Capabilities: promotion.NonNil(capabilities)}, nil
	})
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		Name:          row.Name,
		Project:       row.Project,
		Configuration: configuration.WithNetworkCredential(""),
		Models:        draftModels,
		Slots:         promotion.Map(slots, func(pair storedPair) Slot { return pair.slot }),
		Network:       storedNetwork(configuration),
		Billing:       imported,
	}, nil
}

type storedPair struct {
	name  string
	slot  Slot
	state storedSlot
}

func parseSlotRow(row providers.SlotSnapshot, grant bool) (storedPair, error) {
	var restrictions providers.SlotRestrictions
	if err := json.Unmarshal(row.Restrictions, &restrictions); err != nil {
		return storedPair{}, err
	}
	var limits connection.Limits
	if err := json.Unmarshal(row.Limits, &limits); err != nil {
		return storedPair{}, err
	}
	return storedPair{name: row.Name, slot: Slot{
		Name:          row.Name,
		Default:       row.Default,
		Position:      row.Position,
		Enabled:       row.Enabled,
		Priority:      row.Priority,
		Weight:        row.Weight,
		AllowedModels: promotion.NonNil(restrictions.AllowedModels),
		AllowedRoutes: promotion.NonNil(restrictions.AllowedRoutes),
		Limits:        limits,
		Credential:    storedCredential(grant, row.CredentialID != nil),
	}, state: storedSlot{id: row.ID, credentialID: row.Usable, pluginDigest: row.PluginDigest, profileID: row.ProfileID, allowedAPIKeys: promotion.NonNil(restrictions.AllowedAPIKeys)}}, nil
}

func storedSlots(pairs []storedPair) map[string]storedSlot {
	return promotion.Associate(pairs, func(pair storedPair) (string, storedSlot) { return strings.TrimSpace(pair.name), pair.state })
}

func storedCredential(grant, stored bool) SlotCredential {
	if grant {
		return Grant{}
	}
	if stored {
		return Bound{}
	}
	return Unbound{}
}
