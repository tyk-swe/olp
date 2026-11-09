package provider

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/providers"
)

type planning struct {
	bindings promotion.Bindings
	deps     Deps
}

func Claims(entries []Entry) []promotion.Claim {
	return slices.Concat(promotion.Map(entries, func(entry Entry) []promotion.Claim {
		return slices.Concat(networkClaims(entry), billingClaims(entry), slotClaims(entry))
	})...)
}

func Planner(entries []Entry, bindings promotion.Bindings, deps Deps) promotion.Planner {
	planner := planning{bindings: bindings, deps: deps}
	return promotion.Planner{
		Claims: Claims(entries),
		Plan: func(ctx context.Context, q access.Queryer) ([]promotion.Item, error) {
			return planner.plan(ctx, q, entries)
		},
	}
}

func (p planning) plan(ctx context.Context, q access.Queryer, entries []Entry) ([]promotion.Item, error) {
	directory, err := project.ReadDirectory(ctx, q)
	if err != nil {
		return nil, err
	}
	destinations, err := readDestinations(ctx, q)
	if err != nil {
		return nil, err
	}
	items, err := promotion.TryMap(entries, func(entry Entry) ([]promotion.Item, error) {
		existing, found := destinations[strings.ToLower(entry.Name)]
		return p.planEntry(ctx, q, entry, existing, found, directory)
	})
	if err != nil {
		return nil, err
	}
	return slices.Compact(slices.SortedFunc(slices.Values(slices.Concat(items...)), compareItems)), nil
}

func (p planning) planEntry(ctx context.Context, q access.Queryer, entry Entry, existing destination, found bool, directory promotion.Directory) ([]promotion.Item, error) {
	configuration, plugin, err := p.prepare(ctx, q, entry)
	if err != nil {
		return nil, err
	}
	provider, err := providerItem(entry, configuration, existing, found)
	if err != nil {
		return nil, err
	}
	billing, err := p.billingItems(ctx, q, entry, existing)
	if err != nil {
		return nil, err
	}
	slots, err := p.slotItems(ctx, q, entry, existing)
	if err != nil {
		return nil, err
	}
	return slices.Concat(
		promotion.When(unknownProject(entry, directory), promotion.Blocker(contract.ConfigurationPlanItemKindProvider, entry.Name, "project_unknown")),
		plugin,
		[]promotion.Item{provider},
		networkItems(entry, existing, p.bindings),
		billing,
		slots,
	), nil
}

func unknownProject(entry Entry, directory promotion.Directory) bool {
	if entry.Project == nil || entry.projectDeclared {
		return false
	}
	_, known := directory.ID(*entry.Project)
	return !known
}

func providerItem(entry Entry, configuration providers.Configuration, existing destination, found bool) (promotion.Item, error) {
	kind := contract.ConfigurationPlanItemKindProvider
	switch {
	case !found:
		return promotion.Item{Kind: kind, Key: entry.Name, Action: contract.ConfigurationPlanItemActionCreate, Detail: "draft"}, nil
	case existing.kind != entry.Configuration.Kind:
		return promotion.Conflict(kind, entry.Name, "provider_kind_changed"), nil
	case lower(existing.project) != lower(entry.Project):
		return promotion.Conflict(kind, entry.Name, "provider_project_mismatch"), nil
	}
	same, err := unchanged(entry, configuration, existing.draft)
	if err != nil {
		return promotion.Item{}, err
	}
	if same {
		return promotion.Item{Kind: kind, Key: entry.Name, Action: contract.ConfigurationPlanItemActionNoop}, nil
	}
	return promotion.Item{Kind: kind, Key: entry.Name, Action: contract.ConfigurationPlanItemActionReplace, Detail: "draft"}, nil
}

func unchanged(entry Entry, configuration providers.Configuration, current Entry) (bool, error) {
	named := slices.Collect(func(yield func(string) bool) {
		for _, model := range entry.Models {
			if !yield(model.UpstreamModel) {
				return
			}
		}
	})
	kept := slices.DeleteFunc(slices.Clone(current.Models), func(model providers.DraftModel) bool {
		return !slices.Contains(named, model.UpstreamModel) && !model.Enabled && len(model.Capabilities) == 0
	})
	desired, err := encoded(withParts(entry, configuration, entry.Models))
	if err != nil {
		return false, err
	}
	stored, err := encoded(withParts(current, current.Configuration, kept))
	if err != nil {
		return false, err
	}
	return bytes.Equal(desired, stored), nil
}

func withParts(entry Entry, configuration providers.Configuration, models []providers.DraftModel) Entry {
	return Entry{
		Name:            entry.Name,
		Project:         entry.Project,
		Configuration:   configuration,
		Models:          models,
		Slots:           entry.Slots,
		Network:         entry.Network,
		Billing:         entry.Billing,
		projectDeclared: entry.projectDeclared,
	}
}

func encoded(entry Entry) ([]byte, error) {
	rendered, err := render(entry)
	if err != nil {
		return nil, err
	}
	return json.Marshal(rendered)
}

func (p planning) slotItems(ctx context.Context, q access.Queryer, entry Entry, existing destination) ([]promotion.Item, error) {
	return promotion.TryMap(slices.DeleteFunc(slices.Clone(entry.Slots), func(slot Slot) bool { return !referenced(slot) }), func(slot Slot) (promotion.Item, error) {
		return p.slotItem(ctx, q, entry, slot, existing.slots[slot.Name])
	})
}

func (p planning) slotItem(ctx context.Context, q access.Queryer, entry Entry, slot Slot, current storedSlot) (promotion.Item, error) {
	kind := contract.ConfigurationPlanItemKindCredential
	ref := SlotRef(entry.Name, slot.Name)
	if credential, supplied := p.bindings[ref]; supplied {
		same, err := p.deps.matches(ctx, q, current.credentialID, credential)
		if err != nil {
			return promotion.Item{}, err
		}
		if same {
			return promotion.Item{Kind: kind, Key: string(ref), Action: contract.ConfigurationPlanItemActionReuse}, nil
		}
		return promotion.Item{Kind: kind, Key: string(ref), Action: contract.ConfigurationPlanItemActionBind}, nil
	}
	if current.credentialID != nil && entry.Configuration.Authenticates(current.pluginDigest, current.profileID) {
		return promotion.Item{Kind: kind, Key: string(ref), Action: contract.ConfigurationPlanItemActionReuse}, nil
	}
	if _, grant := slot.Credential.(Grant); grant {
		return promotion.Item{Kind: kind, Key: string(ref), Action: contract.ConfigurationPlanItemActionEnroll, Detail: "grant_enrollment_required"}, nil
	}
	return promotion.Blocker(kind, string(ref), "secret_binding_required"), nil
}

func compareItems(a, b promotion.Item) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Key, b.Key), cmp.Compare(a.Action, b.Action), cmp.Compare(a.Detail, b.Detail))
}

func lower(value *string) string {
	if value == nil {
		return ""
	}
	return strings.ToLower(*value)
}
