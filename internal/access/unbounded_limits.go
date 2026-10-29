package access

// CostBudgetedForRoute checks only boundaries this request spends against.
func (a *Authority) CostBudgetedForRoute(route string) bool {
	return a.Policy.AdmissionLimits().CostBudgeted() ||
		a.DelegationAgentLimits != nil && a.DelegationAgentLimits.Limits.CostBudgeted() ||
		a.BudgetGroupID != nil && a.GroupLimits().CostBudgeted() ||
		a.Policy.RouteLimits[route].CostBudgeted() ||
		a.Policy.EndUserPolicy.Limits(a.EndUserDigest).CostBudgeted() ||
		a.ProjectEndUserPolicy.Limits(a.EndUserDigest).CostBudgeted() ||
		a.InstallationBudget.Limited() || a.OrganizationBudget.Limited() || a.ProjectBudget.Limited() ||
		a.ProjectAttributionBudgets.Matches(a.BudgetAttribution())
}

// TokenBudgeted reports token caps on the boundaries this request spends against.
func (a *Authority) TokenBudgeted(route string) bool {
	return a.Policy.TokensPerMinute != nil ||
		a.DelegationAgentLimits != nil && a.DelegationAgentLimits.Limits.TokensPerMinute != nil ||
		a.BudgetGroupID != nil && a.BudgetGroupTPM != nil ||
		a.Policy.RouteLimits[route].TokensPerMinute != nil ||
		a.Policy.EndUserPolicy.Limits(a.EndUserDigest).TokensPerMinute != nil ||
		a.ProjectEndUserPolicy.Limits(a.EndUserDigest).TokensPerMinute != nil
}

// UnboundedWorkBudgeted reports caps that need a provider-enforced output bound.
// Caller-paid exemptions waive cost caps, never token caps.
func (a *Authority) UnboundedWorkBudgeted(route string, costExempt bool) bool {
	return !costExempt && a.CostBudgetedForRoute(route) || a.TokenBudgeted(route)
}
