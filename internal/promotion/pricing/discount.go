package pricing

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/usage/pricebook"
)

type Discount struct {
	ProviderKind string
	Provider     *string
	Vendor       *string
	Multiplier   string
}

type scope struct{ kind, provider, vendor string }

func (d Discount) scope() scope {
	return scope{kind: d.ProviderKind, provider: folded(d.Provider), vendor: folded(d.Vendor)}
}

func (s scope) key() string {
	return s.kind + "\x00" + s.provider + "\x00" + s.vendor
}

func parseDiscounts(entries []contract.ConfigurationDiscountEntry) ([]Discount, error) {
	discounts := promotion.Map(entries, func(entry contract.ConfigurationDiscountEntry) Discount {
		return Discount{
			ProviderKind: strings.TrimSpace(string(entry.ProviderKind)),
			Provider:     promotion.Trimmed(entry.Provider),
			Vendor:       promotion.Trimmed(entry.VendorId),
			Multiplier:   strings.TrimSpace(entry.Multiplier),
		}
	})
	first := promotion.FirstIndex(discounts, Discount.scope)
	for i, discount := range discounts {
		if err := validateDiscount(i, discount, first[discount.scope()] != i); err != nil {
			return nil, err
		}
	}
	return discounts, nil
}

func validateDiscount(i int, discount Discount, duplicate bool) error {
	field := "discounts." + strconv.Itoa(i)
	switch {
	case discount.Provider == nil && discount.ProviderKind == string(contract.ProviderKindPlugin):
		return access.Invalid(field+".provider", "Scope a plugin provider's discount to that provider.")
	case duplicate:
		return access.Invalid(field, "The document scopes this discount twice.")
	case !nonNegative(discount.Multiplier):
		return access.Invalid(field+".multiplier", "Use a non-negative decimal.")
	default:
		return nil
	}
}

func readDiscounts(ctx context.Context, q access.Queryer, providers promotion.Directory) ([]Discount, error) {
	stored, err := pricebook.ReadDiscountsInForce(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.Map(stored, func(discount pricebook.DiscountInForce) Discount {
		return Discount{ProviderKind: discount.ProviderKind, Provider: nameOf(providers, discount.ProviderID), Vendor: discount.VendorID, Multiplier: discount.Multiplier}
	}), nil
}

func renderDiscounts(discounts []Discount) []contract.ConfigurationDiscountEntry {
	sorted := slices.SortedFunc(slices.Values(discounts), func(a, b Discount) int {
		return strings.Compare(a.scope().key(), b.scope().key())
	})
	return promotion.Map(sorted, func(discount Discount) contract.ConfigurationDiscountEntry {
		return contract.ConfigurationDiscountEntry{
			ProviderKind: contract.ProviderKind(discount.ProviderKind),
			Provider:     discount.Provider,
			VendorId:     discount.Vendor,
			Multiplier:   discount.Multiplier,
		}
	})
}

func multipliers(discounts []Discount) map[scope]string {
	return promotion.Associate(discounts, func(discount Discount) (scope, string) { return discount.scope(), discount.Multiplier })
}

func discountAction(discount Discount, inForce map[scope]string) contract.ConfigurationPlanItemAction {
	multiplier, ok := inForce[discount.scope()]
	if ok && decimalEqual(multiplier, discount.Multiplier) || !ok && decimalEqual(discount.Multiplier, "1") {
		return contract.ConfigurationPlanItemActionNoop
	}
	return contract.ConfigurationPlanItemActionCreate
}

func planDiscounts(ctx context.Context, q access.Queryer, desired []Discount, providers promotion.Directory) ([]promotion.Item, error) {
	current, err := readDiscounts(ctx, q, providers)
	if err != nil {
		return nil, err
	}
	inForce := multipliers(current)
	return promotion.Map(desired, func(discount Discount) promotion.Item {
		return promotion.Item{Kind: contract.ConfigurationPlanItemKindDiscount, Key: discount.scope().key(), Action: discountAction(discount, inForce)}
	}), nil
}

func applyDiscounts(ctx context.Context, tx pgx.Tx, principal access.Principal, desired []Discount, providers promotion.Directory, normalizer pricebook.Normalizer) error {
	if i := slices.IndexFunc(desired, func(discount Discount) bool { return !resolves(providers, discount.Provider) }); i >= 0 {
		return undeclaredDiscount(i)
	}
	current, err := readDiscounts(ctx, tx, providers)
	if err != nil {
		return err
	}
	inForce := multipliers(current)
	for _, discount := range desired {
		if discountAction(discount, inForce) == contract.ConfigurationPlanItemActionNoop {
			continue
		}
		if err := principal.Authorize(access.Settings); err != nil {
			return err
		}
		normalized, normalizeErr := pricebook.NormalizeDiscount(ctx, tx, pricebook.DiscountInput{
			ProviderKind: discount.ProviderKind,
			VendorID:     discount.Vendor,
			ProviderID:   idOf(providers, discount.Provider),
			Multiplier:   &discount.Multiplier,
		}, normalizer)
		if normalizeErr != nil {
			return normalizeErr
		}
		if _, err := pricebook.CreateDiscount(ctx, tx, principal.UserID(), normalized); err != nil {
			return err
		}
	}
	return nil
}

func undeclaredDiscount(i int) error {
	return undeclaredProvider("discounts."+strconv.Itoa(i)+".provider", "Discount "+strconv.Itoa(i))
}
