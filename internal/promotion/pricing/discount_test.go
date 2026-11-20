package pricing

import (
	"testing"

	"github.com/tyk-swe/olp/internal/management/contract"
)

func TestExplicitProviderDiscountOverridesKindDiscount(t *testing.T) {
	t.Parallel()
	provider := "promotion-fixture"
	kind := string(contract.ProviderKindBedrock)
	desired := Discount{ProviderKind: kind, Provider: &provider, Multiplier: "1"}
	kindDiscount := Discount{ProviderKind: kind, Multiplier: "0.8"}

	for _, tc := range []struct {
		name    string
		current []Discount
		want    contract.ConfigurationPlanItemAction
	}{
		{
			name:    "missing provider override",
			current: []Discount{kindDiscount},
			want:    contract.ConfigurationPlanItemActionCreate,
		},
		{
			name:    "equal provider override",
			current: []Discount{kindDiscount, {ProviderKind: kind, Provider: &provider, Multiplier: "1.00"}},
			want:    contract.ConfigurationPlanItemActionNoop,
		},
		{
			name:    "different provider override",
			current: []Discount{kindDiscount, {ProviderKind: kind, Provider: &provider, Multiplier: "0.9"}},
			want:    contract.ConfigurationPlanItemActionCreate,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := discountAction(desired, multipliers(tc.current)); got != tc.want {
				t.Errorf("planning the provider override = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPromotionDiscountsUseStorablePrecision(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		multiplier string
		valid      bool
	}{
		{multiplier: "1.000000000000", valid: true},
		{multiplier: "1.0000000000000", valid: false},
	} {
		t.Run(tc.multiplier, func(t *testing.T) {
			_, err := Parse(contract.ConfigurationDocument{
				Discounts: &[]contract.ConfigurationDiscountEntry{{
					ProviderKind: contract.ProviderKindBedrock,
					Multiplier:   tc.multiplier,
				}},
			})
			if (err == nil) != tc.valid {
				t.Errorf("parse multiplier %q: got error %v, want accepted = %t", tc.multiplier, err, tc.valid)
			}
		})
	}
}
