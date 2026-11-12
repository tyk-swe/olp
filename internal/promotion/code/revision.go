package code

import (
	"context"
	"encoding/json"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/provider"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routing"
)

type portableRevision struct {
	configuration []byte
	models        []byte
	slots         []byte
}

func portablePaid(ctx context.Context, q access.Queryer, frozen map[string]routing.Provider) (map[string]PaidConnection, error) {
	connections, err := promotion.TryMap(slices.Sorted(maps.Keys(frozen)), func(id string) (PaidConnection, error) { return portablePaidConnection(ctx, q, frozen[id]) })
	if err != nil {
		return nil, err
	}
	return promotion.Index(connections, func(c PaidConnection) string { return c.Provider.Name }), nil
}

func portablePaidConnection(ctx context.Context, q access.Queryer, frozen routing.Provider) (PaidConnection, error) {
	if frozen.PendingCommitment != nil {
		return PaidConnection{}, access.Invalid("code.paid_connections", "Publish the pending monetary commitment before exporting this paid provider.")
	}
	stored, err := providers.ReadExportedRevision(ctx, q, frozen.RevisionID, frozen.ID)
	if err != nil {
		return PaidConnection{}, err
	}
	revision, err := revisionOf(frozen, stored)
	if err != nil {
		return PaidConnection{}, err
	}
	entry, err := provider.FromRevision(stored.Provider, &stored.Project, revision.configuration, revision.models, revision.slots)
	if err != nil {
		return PaidConnection{}, err
	}
	restricted := slices.DeleteFunc(slices.Clone(frozen.Slots), func(slot routing.Slot) bool { return len(slot.AllowedAPIKeys) == 0 })
	refs := promotion.Associate(restricted, func(slot routing.Slot) (string, []string) {
		return slot.Name, promotion.Map(slot.AllowedAPIKeys, func(id string) string { return codeRef("key", id) })
	})
	return PaidConnection{Provider: entry, SlotKeyRefs: refs}, nil
}

func revisionOf(frozen routing.Provider, stored providers.ExportedRevision) (portableRevision, error) {
	upstream := slices.Compact(slices.Sorted(func(yield func(string) bool) {
		for _, capability := range frozen.Capabilities {
			if !yield(capability.Model) {
				return
			}
		}
	}))
	models, err := json.Marshal(promotion.Map(upstream, func(model string) routing.RevisionModel { return modelOf(model, frozen, stored.Models) }))
	if err != nil {
		return portableRevision{}, err
	}
	slots, err := json.Marshal(promotion.Map(frozen.Slots, func(slot routing.Slot) routing.RevisionSlot {
		return routing.RevisionSlot{Slot: slot, Default: slot.ID == frozen.DefaultSlotID}
	}))
	return portableRevision{configuration: stored.Configuration, models: models, slots: slots}, err
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
