package routing

// UnboundedWorkBudgeted reports route and provider caps that need a bound on
// provider work. The caller supplies only a slot eligible for this request.
func UnboundedWorkBudgeted(provider *Provider, slot *Slot, route *Route) bool {
	return route != nil && route.Budget != nil && (route.Budget.DailyCostLimit != nil || route.Budget.MonthlyCostLimit != nil) ||
		provider != nil && provider.Limits != nil && (provider.Limits.TokensPerMinute != nil || provider.Limits.Costs() != nil) ||
		slot != nil && (slot.TokensPerMinute != nil || slot.Costs() != nil)
}
