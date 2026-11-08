package code

import (
	"context"
	"encoding/json"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/provider"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routing"
)

type portableRevision struct {
	configuration []byte
	models        []byte
	slots         []byte
}

func portablePaid(ctx context.Context, q access.Queryer, frozen map[string]routing.Provider) (map[string]PaidConnection, error) {
	ids := sortedKeys(frozen)
	connections, err := promotion.TryMapIndex(ids, func(_ int, id string) (PaidConnection, error) { return portablePaidConnection(ctx, q, frozen[id]) })
	if err != nil {
		return nil, err
	}
	return keyed(promotion.Map(connections, func(c PaidConnection) string { return c.Provider.Name }), connections), nil
}

func portablePaidConnection(ctx context.Context, q access.Queryer, frozen routing.Provider) (PaidConnection, error) {
	if frozen.PendingCommitment != nil {
		return PaidConnection{}, access.Invalid("code.paid_connections", "Publish the pending monetary commitment before exporting this paid provider.")
	}
	var labels []routing.RevisionModel
	var name, project string
	if err := q.QueryRow(ctx, `SELECT p.name,j.name,v.models FROM olp.provider_revisions v JOIN olp.providers p ON p.id=v.provider_id JOIN olp.projects j ON j.id=p.project_id WHERE v.id=$1 AND p.id=$2`, frozen.RevisionID, frozen.ID).Scan(&name, &project, &labels); err != nil {
		return PaidConnection{}, err
	}
	revision, err := revisionOf(frozen, labels)
	if err != nil {
		return PaidConnection{}, err
	}
	entry, err := provider.FromRevision(name, &project, revision.configuration, revision.models, revision.slots)
	if err != nil {
		return PaidConnection{}, err
	}
	restricted := slices.DeleteFunc(slices.Clone(frozen.Slots), func(slot routing.Slot) bool { return len(slot.AllowedAPIKeys) == 0 })
	refs := keyed(promotion.Map(restricted, func(slot routing.Slot) string { return slot.Name }), promotion.Map(restricted, func(slot routing.Slot) []string {
		return promotion.Map(slot.AllowedAPIKeys, func(id string) string { return "code/key/" + id })
	}))
	return PaidConnection{Provider: entry, SlotKeyRefs: refs}, nil
}

func revisionOf(frozen routing.Provider, labels []routing.RevisionModel) (portableRevision, error) {
	configuration, err := json.Marshal(configurationOf(frozen))
	if err != nil {
		return portableRevision{}, err
	}
	upstream := slices.Compact(slices.Sorted(func(yield func(string) bool) {
		for _, capability := range frozen.Capabilities {
			if !yield(capability.Model) {
				return
			}
		}
	}))
	models, err := json.Marshal(promotion.Map(upstream, func(model string) routing.RevisionModel { return modelOf(model, frozen, labels) }))
	if err != nil {
		return portableRevision{}, err
	}
	slots, err := json.Marshal(promotion.Map(frozen.Slots, func(slot routing.Slot) routing.RevisionSlot {
		return routing.RevisionSlot{Slot: slot, Default: slot.ID == frozen.DefaultSlotID}
	}))
	return portableRevision{configuration: configuration, models: models, slots: slots}, err
}

func modelOf(model string, frozen routing.Provider, labels []routing.RevisionModel) routing.RevisionModel {
	capabilities := slices.Collect(func(yield func(routing.RevisionCapability) bool) {
		for _, capability := range frozen.Capabilities {
			if capability.Model == model && !yield(routing.RevisionCapability{Operation: capability.Operation, Surface: capability.Surface, Mode: capability.Mode, Source: "certified"}) {
				return
			}
		}
	})
	return routing.RevisionModel{UpstreamModel: model, DisplayName: displayName(model, labels), Capabilities: capabilities}
}

func displayName(model string, labels []routing.RevisionModel) string {
	at := slices.IndexFunc(labels, func(m routing.RevisionModel) bool { return m.UpstreamModel == model })
	if at < 0 {
		return model
	}
	return labels[at].DisplayName
}

func configurationOf(frozen routing.Provider) providers.Configuration {
	return providers.Configuration{
		Options: providers.Options{
			Limits: limitsOf(frozen.Limits), Commitment: frozen.Commitment, HealthProbe: frozen.HealthProbe, TuningDatasetURIPrefixes: frozen.TuningDatasetURIPrefixes,
			DocumentURIPrefixes: frozen.DocumentURIPrefixes, Network: frozen.Network, SemanticHeaders: frozen.SemanticHeaders, QuerySettings: frozen.QuerySettings,
			OperationDefaults: frozen.OperationDefaults, Bindings: frozen.Bindings, CredentialHeaders: promotion.NonNil(slices.Clone(frozen.CredentialHeaders)),
			Models: maps.Collect(maps.All(frozen.Models)), ParameterDefaults: maps.Collect(maps.All(frozen.ParameterDefaults)), VendorID: nonEmpty(frozen.VendorID), PluginOptions: frozen.PluginOptions,
		},
		Identity: providerconfig.Identity[*string]{
			CredentialSource: frozen.CredentialSource, ProfileID: frozen.ProfileID, ProfileRevision: frozen.ProfileRevision, Kind: frozen.Kind, AuthMode: frozen.AuthMode,
			Endpoint: nonEmpty(frozen.Endpoint), CloudRegion: nonEmpty(frozen.CloudRegion), CloudProject: nonEmpty(frozen.CloudProject), BatchStorage: frozen.BatchStorage,
			Deployment: nonEmpty(frozen.Deployment), APIVersion: nonEmpty(frozen.APIVersion),
		},
	}
}

func limitsOf(limits *routing.Limits) *providers.Limits {
	if limits == nil {
		return nil
	}
	return &providers.Limits{MaxConcurrency: limits.MaxConcurrency, RequestsPerMinute: limits.RequestsPerMinute, TokensPerMinute: limits.TokensPerMinute, Supply: limits.Supply}
}

func nonEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
