package access

import (
	"fmt"
	"testing"
)

func TestAttributionBudgetValidation(t *testing.T) {
	positive := "0.000000000001"
	zero := "0"
	for name, budgets := range map[string]AttributionBudgets{
		"label syntax":   {"bad label": {"core": {DailyCostLimit: &positive}}},
		"value syntax":   {"team": {"private email@example.com": {DailyCostLimit: &positive}}},
		"empty values":   {"team": {}},
		"unlimited pair": {"team": {"core": {}}},
		"zero cap":       {"team": {"core": {WeeklyCostLimit: &zero}}},
	} {
		t.Run(name, func(t *testing.T) {
			if budgets.Validate() == nil {
				t.Fatal("invalid budget accepted")
			}
		})
	}
	budgets := AttributionBudgets{"team": {}}
	for i := range 64 {
		budgets["team"][fmt.Sprintf("v%d", i)] = BudgetPolicy{MonthlyCostLimit: &positive}
	}
	if err := budgets.Validate(); err != nil {
		t.Fatal(err)
	}
	budgets["team"]["extra"] = BudgetPolicy{DailyCostLimit: &positive}
	if budgets.Validate() == nil {
		t.Fatal("more than 64 pairs accepted")
	}
	if !AttributionBudgets(nil).Equal(AttributionBudgets{}) {
		t.Fatal("empty maps should clear identically")
	}
}

func TestAttributionBudgetMatchingIsExactAndAllocationFree(t *testing.T) {
	cap := "1"
	budgets := AttributionBudgets{"team": {"core": {WeeklyCostLimit: &cap}}}
	for _, labels := range []map[string]string{nil, {}, {"team": "edge"}, {"Team": "core"}} {
		if budgets.Matches(labels) {
			t.Fatal("unmatched labels acquired a cap")
		}
	}
	labels := map[string]string{"team": "core"}
	if !budgets.Matches(labels) {
		t.Fatal("matching cap omitted")
	}
	if allocations := testing.AllocsPerRun(100, func() { budgets.Matches(labels) }); allocations != 0 {
		t.Fatalf("matching allocated: %v", allocations)
	}
}
