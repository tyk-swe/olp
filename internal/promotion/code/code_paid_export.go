package code

import (
	"context"
	"encoding/json"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/providerpromotion"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/routing"
)

func exportCodePaidReferences(ctx context.Context, q access.Queryer) (map[string]promotion.CodePaidTarget, error) {
	rows, err := q.Query(ctx, `SELECT p.id::text,p.name,s.id::text,s.name FROM olp.providers p JOIN olp.provider_slots s ON s.provider_id=p.id`)
	if err != nil {
		return nil, err
	}
	type reference struct{ ProviderID, Provider, SlotID, Slot string }
	refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (reference, error) {
		var out reference
		err := row.Scan(&out.ProviderID, &out.Provider, &out.SlotID, &out.Slot)
		return out, err
	})
	if err != nil {
		return nil, err
	}
	return maps.Collect(func(yield func(string, promotion.CodePaidTarget) bool) {
		for _, ref := range refs {
			if !yield(ref.ProviderID+"/"+ref.SlotID, promotion.CodePaidTarget{Provider: ref.Provider, Slot: ref.Slot, CredentialRef: promotion.CredentialRef(ref.Provider, ref.Slot)}) {
				return
			}
		}
	}), nil
}
func exportCodePaidPolicy(route codemode.Route, refs map[string]promotion.CodePaidTarget) (promotion.CodeRoutePolicy, error) {
	direct, err := exportCodePaidTargets(route.PaidTargets, refs)
	if err != nil {
		return promotion.CodeRoutePolicy{}, err
	}
	fallback, fallbackError := exportCodePaidTargets(route.PaidFallbackTargets, refs)
	if fallbackError != nil {
		return promotion.CodeRoutePolicy{}, fallbackError
	}
	return codePolicyPaid(codePolicy(route), direct, fallback), nil
}
func exportCodePaidTargets(references map[string]codemode.PaidTarget, refs map[string]promotion.CodePaidTarget) (map[string]promotion.CodePaidTarget, error) {
	targets, err := mapCodeResult(slices.Collect(maps.Keys(references)), func(model string) (struct {
		Model  string
		Target promotion.CodePaidTarget
	}, error) {
		target := references[model]
		ref, ok := refs[target.ProviderID+"/"+target.SlotID]
		if !ok {
			return struct {
				Model  string
				Target promotion.CodePaidTarget
			}{}, access.Invalid("code.paid_targets", "A paid target no longer has a portable provider and slot reference.")
		}
		return struct {
			Model  string
			Target promotion.CodePaidTarget
		}{Model: model, Target: ref}, nil
	})
	if err != nil {
		return nil, err
	}
	return maps.Collect(func(yield func(string, promotion.CodePaidTarget) bool) {
		for _, target := range targets {
			if !yield(target.Model, target.Target) {
				return
			}
		}
	}), nil
}
func exportCodePaid(ctx context.Context, q access.Queryer, frozen map[string]routing.Provider) (map[string]promotion.CodePaidConnection, error) {
	type named struct {
		Name  string
		Value promotion.CodePaidConnection
	}
	entries, err := mapCodeResult(slices.Collect(maps.Values(frozen)), func(provider routing.Provider) (named, error) {
		var labels []routing.RevisionModel
		var name, project string
		if err := q.QueryRow(ctx, `SELECT p.name,j.name,v.models FROM olp.provider_revisions v JOIN olp.providers p ON p.id=v.provider_id JOIN olp.projects j ON j.id=p.project_id WHERE v.id=$1 AND p.id=$2`, provider.RevisionID, provider.ID).Scan(&name, &project, &labels); err != nil {
			return named{}, err
		}
		configuration, models, slots, err := codePaidRevision(provider, labels)
		if err != nil {
			return named{}, err
		}
		entry, err := providerpromotion.PortableProviderRevision(promotion.ProviderEntry{Name: name, Project: &project}, configuration, models, slots)
		if err != nil {
			return named{}, err
		}
		var revisionSlots []routing.RevisionSlot
		if err := json.Unmarshal(slots, &revisionSlots); err != nil {
			return named{}, err
		}
		return named{Name: name, Value: promotion.CodePaidConnection{Provider: entry, SlotKeyRefs: maps.Collect(func(yield func(string, []string) bool) {
			for _, slot := range revisionSlots {
				if len(slot.AllowedAPIKeys) > 0 && !yield(slot.Name, promotion.MapCode(slot.AllowedAPIKeys, func(id string) string { return "code/key/" + id })) {
					return
				}
			}
		})}}, nil
	})
	if err != nil {
		return nil, err
	}
	return maps.Collect(func(yield func(string, promotion.CodePaidConnection) bool) {
		for _, entry := range entries {
			if !yield(entry.Name, entry.Value) {
				return
			}
		}
	}), nil
}
