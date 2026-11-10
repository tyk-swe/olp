package release

import (
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/limits/ledger"
	"github.com/tyk-swe/olp/internal/routing"
)

// supplyBudgets lists every spend cap the snapshot declares, for
// reconciliation to keep current: capped connections, capped credential slots
// and routes with a budget.
func supplyBudgets(s *routing.Snapshot) []ledger.SupplyBudget {
	var budgets []ledger.SupplyBudget
	add := func(ownerID, kind string, costs *routing.CostLimits) {
		if costs != nil {
			budgets = append(budgets, ledger.SupplyBudget{OwnerID: ownerID, Kind: kind, Daily: costs.DailyCostLimit, Monthly: costs.MonthlyCostLimit})
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.Providers)) {
		provider := s.Providers[id]
		if provider.Limits != nil {
			add(provider.ID, "connection", provider.Limits.Costs())
		}
		for _, slot := range provider.Slots {
			add(slot.ID, "slot", slot.Supply.Costs())
		}
	}
	for _, slug := range slices.Sorted(maps.Keys(s.Routes)) {
		route := s.Routes[slug]
		add(route.ID, "route", route.Budget)
	}
	return budgets
}
