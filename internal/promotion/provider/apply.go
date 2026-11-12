package provider

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

type applying struct {
	bindings  promotion.Bindings
	deps      Deps
	createdBy string
}

func Applier(entries []Entry, bindings promotion.Bindings, deps Deps) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		directory, err := project.ReadDirectory(ctx, tx)
		if err != nil {
			return err
		}
		destinations, err := readDestinations(ctx, tx)
		if err != nil {
			return err
		}
		applier := applying{bindings: bindings, deps: deps, createdBy: p.UserID()}
		for _, entry := range entries {
			existing, found := destinations[strings.ToLower(entry.Name)]
			if err := applier.apply(ctx, tx, entry, existing, found, directory); err != nil {
				return err
			}
		}
		return nil
	}
}

func (a applying) apply(ctx context.Context, tx pgx.Tx, entry Entry, existing destination, found bool, directory promotion.Directory) error {
	prepared, err := connection.Prepare(ctx, a.deps.Catalogue.ConnectionPolicy(tx), entry.Configuration)
	if err != nil {
		return err
	}
	desired := withParts(entry, prepared.Document(), entry.Models)
	if !found {
		return a.create(ctx, tx, desired, prepared, directory)
	}
	same, err := unchanged(entry, prepared.Document(), existing.draft)
	if err != nil {
		return err
	}
	if same {
		return a.rebind(ctx, tx, desired, prepared, existing)
	}
	return a.replace(ctx, tx, desired, prepared, existing)
}

func (a applying) create(ctx context.Context, tx pgx.Tx, entry Entry, prepared connection.Prepared, directory promotion.Directory) error {
	projectID, err := projectID(entry, directory)
	if err != nil {
		return err
	}
	created, err := a.deps.Catalogue.CreateDraft(ctx, tx, providers.Draft{Name: entry.Name, ProjectID: projectID, Configuration: prepared, CreatedBy: a.createdBy})
	if err != nil {
		return err
	}
	return a.replace(ctx, tx, entry, prepared, destination{id: created.ID})
}

func projectID(entry Entry, directory promotion.Directory) (*string, error) {
	if entry.Project == nil {
		return nil, nil
	}
	id, known := directory.ID(*entry.Project)
	if !known {
		return nil, promotion.UnknownProject(*entry.Project)
	}
	return &id, nil
}

func (a applying) rebind(ctx context.Context, tx pgx.Tx, entry Entry, prepared connection.Prepared, target destination) error {
	for _, slot := range entry.Slots {
		if err := a.rebindSlot(ctx, tx, entry, slot, target); err != nil {
			return err
		}
	}
	if err := a.bindNetwork(ctx, tx, entry, prepared, target, true); err != nil {
		return err
	}
	return a.applyBilling(ctx, tx, entry, target)
}

func (a applying) rebindSlot(ctx context.Context, tx pgx.Tx, entry Entry, slot Slot, target destination) error {
	current, present := target.slots[slot.Name]
	if !present {
		return nil
	}
	fresh, err := a.replacement(ctx, tx, entry, slot, target.id, current.credentialID)
	if err != nil || fresh == nil {
		return err
	}
	return a.deps.Catalogue.BindDraftCredential(ctx, tx, target.id, current.id, *fresh)
}

func (a applying) replace(ctx context.Context, tx pgx.Tx, entry Entry, prepared connection.Prepared, target destination) error {
	if err := a.bindNetwork(ctx, tx, entry, prepared, target, false); err != nil {
		return err
	}
	if err := a.applyBilling(ctx, tx, entry, target); err != nil {
		return err
	}
	slots, err := promotion.TryMap(entry.Slots, func(slot Slot) (providers.DraftSlot, error) {
		return a.resolveSlot(ctx, tx, entry, slot, target)
	})
	if err != nil {
		return err
	}
	return a.deps.Catalogue.StageDraftInventory(ctx, tx, target.id, entry.Models, slots)
}

func (a applying) resolveSlot(ctx context.Context, tx pgx.Tx, entry Entry, slot Slot, target destination) (providers.DraftSlot, error) {
	current, present := target.slots[slot.Name]
	held := held(current, present, slot)
	fresh, err := a.replacement(ctx, tx, entry, slot, target.id, held)
	if err != nil {
		return providers.DraftSlot{}, err
	}
	return providers.DraftSlot{
		ID:           cmp.Or(current.id, access.NewID()),
		Default:      slot.Default,
		Position:     slot.Position,
		Name:         slot.Name,
		Enabled:      slot.Enabled,
		Priority:     slot.Priority,
		Weight:       slot.Weight,
		CredentialID: cmp.Or(fresh, held),
		Restrictions: providers.SlotRestrictions{AllowedAPIKeys: current.allowedAPIKeys, AllowedModels: slot.AllowedModels, AllowedRoutes: slot.AllowedRoutes},
		Limits:       slot.Limits,
	}, nil
}

func held(current storedSlot, present bool, slot Slot) *string {
	if !present || !referenced(slot) {
		return nil
	}
	return current.credentialID
}

func (a applying) replacement(ctx context.Context, tx pgx.Tx, entry Entry, slot Slot, providerID string, current *string) (*string, error) {
	credential, supplied := a.bindings[SlotRef(entry.Name, slot.Name)]
	if !supplied || !referenced(slot) {
		return nil, nil
	}
	same, err := a.deps.matches(ctx, tx, current, credential)
	if err != nil || same {
		return nil, err
	}
	id, err := a.deps.store(ctx, tx, providerID, credential)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func (d Deps) matches(ctx context.Context, q access.Queryer, credentialID *string, credential promotion.Credential) (bool, error) {
	if credentialID == nil {
		return false, nil
	}
	switch bound := credential.(type) {
	case promotion.Sealed:
		return d.Catalogue.CredentialEquals(ctx, q, *credentialID, bound.Secret)
	case promotion.External:
		return d.Catalogue.ExternalCredentialEquals(ctx, q, *credentialID, bound.Reference)
	}
	return false, fmt.Errorf("unsupported credential binding %T", credential)
}

func (d Deps) store(ctx context.Context, tx pgx.Tx, providerID string, credential promotion.Credential) (string, error) {
	switch bound := credential.(type) {
	case promotion.Sealed:
		id, _, err := d.Catalogue.StoreCredential(ctx, tx, providerID, bound.Secret)
		return id, err
	case promotion.External:
		id, _, err := d.Catalogue.StoreExternalCredential(ctx, tx, providerID, bound.Reference)
		return id, err
	}
	return "", fmt.Errorf("unsupported credential binding %T", credential)
}
