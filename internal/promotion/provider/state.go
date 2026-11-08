package provider

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/billing"
	"github.com/tyk-swe/olp/internal/providers"
)

type destination struct {
	id        string
	kind      string
	project   *string
	network   *string
	billing   *billing.ImportState
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

type providerRow struct {
	ID                    string
	Name                  string
	Project               *string
	Kind                  string
	Configuration         []byte
	Network               *string
	Imported              bool
	RevisionConfiguration []byte
	RevisionModels        []byte
	RevisionSlots         []byte
}

type modelRow struct {
	ProviderID    string
	UpstreamModel string
	DisplayName   string
	Enabled       bool
	Capabilities  []byte
}

type slotRow struct {
	ProviderID   string
	Name         string
	ID           string
	Default      bool
	Position     int
	Enabled      bool
	Priority     int
	Weight       int64
	CredentialID *string
	Restrictions []byte
	Limits       []byte
	Usable       *string
	PluginDigest string
	ProfileID    string
}

const providersQuery = `SELECT p.id::text,p.name,pr.name,p.kind,p.configuration,
	(SELECT c.id::text FROM olp.provider_network_credentials c WHERE c.id::text=p.configuration#>>'{options,network,credential_id}' AND c.provider_id=p.id AND c.revoked_at IS NULL),
	EXISTS (SELECT 1 FROM olp.provider_billing_imports bi WHERE bi.provider_id=p.id),
	r.configuration,r.models,r.slots
	FROM olp.providers p LEFT JOIN olp.projects pr ON pr.id=p.project_id
	LEFT JOIN olp.provider_revisions r ON r.id=p.active_revision_id AND p.state IN ('active','disabled')`

const modelsQuery = `SELECT provider_id::text,upstream_model,display_name,enabled,capabilities FROM olp.provider_models ORDER BY provider_id,upstream_model`

const slotsQuery = `SELECT s.provider_id::text,s.name,s.id::text,s.is_default,s.position,s.enabled,s.priority,s.weight,s.credential_id::text,s.restrictions,s.limits,
	c.id::text,coalesce(c.plugin_digest,''),coalesce(c.profile_id,'')
	FROM olp.provider_slots s LEFT JOIN olp.provider_credentials c ON c.id=s.credential_id AND c.revoked_at IS NULL
	AND NOT EXISTS (SELECT 1 FROM olp.provider_grants g WHERE g.credential_id=c.id AND g.lapsed_at IS NOT NULL)
	ORDER BY s.provider_id,s.position`

func Read(ctx context.Context, q access.Queryer) ([]Entry, error) {
	destinations, err := readDestinations(ctx, q)
	if err != nil {
		return nil, err
	}
	return mapSlice(slices.Collect(maps.Values(destinations)), func(_ int, existing destination) Entry {
		if existing.published != nil {
			return *existing.published
		}
		return existing.draft
	}), nil
}

func readDestinations(ctx context.Context, q access.Queryer) (map[string]destination, error) {
	rows, err := collect[providerRow](ctx, q, providersQuery)
	if err != nil {
		return nil, err
	}
	models, err := collect[modelRow](ctx, q, modelsQuery)
	if err != nil {
		return nil, err
	}
	slots, err := collect[slotRow](ctx, q, slotsQuery)
	if err != nil {
		return nil, err
	}
	modelsOf := groupBy(models, func(model modelRow) string { return model.ProviderID })
	slotsOf := groupBy(slots, func(slot slotRow) string { return slot.ProviderID })
	destinations, err := each(rows, func(_ int, row providerRow) (destination, error) {
		return readDestination(ctx, q, row, modelsOf[row.ID], slotsOf[row.ID])
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

func readDestination(ctx context.Context, q access.Queryer, row providerRow, models []modelRow, slots []slotRow) (destination, error) {
	imported, err := readImport(ctx, q, row)
	if err != nil {
		return destination{}, err
	}
	var configuration providers.Configuration
	if err := json.Unmarshal(row.Configuration, &configuration); err != nil {
		return destination{}, err
	}
	stored, err := each(slots, func(_ int, slot slotRow) (storedPair, error) { return parseSlotRow(slot, configuration.Grant()) })
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

func readImport(ctx context.Context, q access.Queryer, row providerRow) (*billing.ImportState, error) {
	if !row.Imported {
		return nil, nil
	}
	return billing.ReadImportState(ctx, q, row.ID)
}

func draftEntry(row providerRow, configuration providers.Configuration, models []modelRow, slots []storedPair, imported *Billing) (Entry, error) {
	draftModels, err := each(models, func(_ int, model modelRow) (providers.DraftModel, error) {
		var capabilities []providers.CapabilityInput
		if err := json.Unmarshal(model.Capabilities, &capabilities); err != nil {
			return providers.DraftModel{}, err
		}
		return providers.DraftModel{UpstreamModel: model.UpstreamModel, DisplayName: model.DisplayName, Enabled: model.Enabled, Capabilities: orEmpty(capabilities)}, nil
	})
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		Name:          row.Name,
		Project:       row.Project,
		Configuration: withNetworkCredential(configuration, ""),
		Models:        draftModels,
		Slots:         mapSlice(slots, func(_ int, pair storedPair) Slot { return pair.slot }),
		Network:       storedNetwork(configuration),
		Billing:       imported,
	}, nil
}

type storedPair struct {
	name  string
	slot  Slot
	state storedSlot
}

func parseSlotRow(row slotRow, grant bool) (storedPair, error) {
	var restrictions providers.SlotRestrictions
	if err := json.Unmarshal(row.Restrictions, &restrictions); err != nil {
		return storedPair{}, err
	}
	var limits providers.Limits
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
		AllowedModels: orEmpty(restrictions.AllowedModels),
		AllowedRoutes: orEmpty(restrictions.AllowedRoutes),
		Limits:        limits,
		Credential:    storedCredential(grant, row.CredentialID != nil),
	}, state: storedSlot{id: row.ID, credentialID: row.Usable, pluginDigest: row.PluginDigest, profileID: row.ProfileID, allowedAPIKeys: orEmpty(restrictions.AllowedAPIKeys)}}, nil
}

func storedSlots(pairs []storedPair) map[string]storedSlot {
	return maps.Collect(func(yield func(string, storedSlot) bool) {
		for _, pair := range pairs {
			if !yield(strings.TrimSpace(pair.name), pair.state) {
				return
			}
		}
	})
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

func collect[T any](ctx context.Context, q access.Queryer, query string) ([]T, error) {
	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[T])
}

func groupBy[T any](rows []T, key func(T) string) map[string][]T {
	groups := map[string][]T{}
	for _, row := range rows {
		groups[key(row)] = append(groups[key(row)], row)
	}
	return groups
}
