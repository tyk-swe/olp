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
