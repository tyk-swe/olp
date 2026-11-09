package code

import (
	"context"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/provider"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routing"
)

type PaidTarget struct {
	Provider      string
	Slot          string
	CredentialRef string
}

func targetOf(e contract.ConfigurationCodePaidTarget) PaidTarget {
	return PaidTarget{Provider: e.Provider, Slot: e.Slot, CredentialRef: e.CredentialRef}
}

func (t PaidTarget) render() contract.ConfigurationCodePaidTarget {
	return contract.ConfigurationCodePaidTarget{Provider: t.Provider, Slot: t.Slot, CredentialRef: t.CredentialRef}
}

func resolveTargets(ctx context.Context, q access.Queryer, targets map[string]PaidTarget, project string, credentials map[string]string) (map[string]codemode.PaidTarget, error) {
	return tryMappedValues(targets, func(_ string, t PaidTarget) (codemode.PaidTarget, error) {
		return resolveTarget(ctx, q, t, project, credentials)
	})
}

func resolveTarget(ctx context.Context, q access.Queryer, t PaidTarget, project string, credentials map[string]string) (codemode.PaidTarget, error) {
	credential, err := bound(t.CredentialRef, credentials)
	if err != nil {
		return codemode.PaidTarget{}, err
	}
	target, err := providers.ReadPublishedSlot(ctx, q, t.Provider, project, t.Slot, credential)
	if err != nil {
		return codemode.PaidTarget{}, err
	}
	if target == nil {
		return codemode.PaidTarget{}, access.Invalid("code.paid_targets", "Bind the current credential of an enabled published destination provider slot.")
	}
	return *target, nil
}

func validLimits(ctx context.Context, q access.Queryer, frozen routing.Provider, slot routing.Slot, project string) error {
	live, err := providers.ReadSlotLimits(ctx, q, frozen.ID, slot.ID, project)
	if err != nil {
		return err
	}
	if !routing.CodePaidLimitsMatch(live.Configuration.Options.Limits, frozen.Limits, live.Slot, slot) {
		return access.Invalid("code.paid_connections.limits", "Frozen paid provider and slot limits must match the destination's live serving policy.")
	}
	return nil
}

func slotKey(providerID, slotID string) string { return promotion.TupleKey(providerID, slotID) }

func readSlotNames(ctx context.Context, q access.Queryer) (map[string]PaidTarget, error) {
	names, err := providers.ReadSlotNames(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.Associate(names, func(n providers.SlotName) (string, PaidTarget) {
		return slotKey(n.ProviderID, n.SlotID), PaidTarget{Provider: n.Provider, Slot: n.Slot, CredentialRef: string(provider.SlotRef(n.Provider, n.Slot))}
	}), nil
}

func portableTargets(targets map[string]codemode.PaidTarget, names map[string]PaidTarget) (map[string]PaidTarget, error) {
	return tryMappedValues(targets, func(_ string, t codemode.PaidTarget) (PaidTarget, error) {
		target, ok := names[slotKey(t.ProviderID, t.SlotID)]
		if !ok {
			return PaidTarget{}, access.Invalid("code.paid_targets", "A paid target no longer has a portable provider and slot reference.")
		}
		return target, nil
	})
}
