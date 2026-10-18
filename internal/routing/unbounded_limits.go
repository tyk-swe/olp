package routing

// UnboundedWorkBudgeted reports route and provider caps that need a bound on
// provider work. The caller supplies only a slot eligible for this request.
// Caller-paid exemptions waive cost caps, never token caps.
func UnboundedWorkBudgeted(provider *Provider, slot *Slot, route *Route, costExempt bool) bool {
	if provider != nil && provider.Limits != nil && provider.Limits.TokensPerMinute != nil || slot != nil && slot.TokensPerMinute != nil {
		return true
	}
	return !costExempt && (route != nil && route.Budget != nil && (route.Budget.DailyCostLimit != nil || route.Budget.MonthlyCostLimit != nil) ||
		provider != nil && provider.Limits != nil && provider.Limits.Costs() != nil ||
		slot != nil && slot.Costs() != nil)
}
