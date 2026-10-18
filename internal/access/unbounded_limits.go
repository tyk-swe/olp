package access

// CostBudgetedForRoute checks only boundaries this request spends against.
func (a *Authority) CostBudgetedForRoute(route string) bool {
	return a.Policy.AdmissionLimits().CostBudgeted() ||
		a.BudgetGroupID != nil && a.GroupLimits().CostBudgeted() ||
		a.Policy.RouteLimits[route].CostBudgeted() ||
		a.Policy.EndUserPolicy.Limits(a.EndUserDigest).CostBudgeted() ||
		a.ProjectEndUserPolicy.Limits(a.EndUserDigest).CostBudgeted() ||
		a.InstallationBudget.Limited() || a.OrganizationBudget.Limited() || a.ProjectBudget.Limited() ||
		a.ProjectAttributionBudgets.Matches(a.Attribution)
}

// UnboundedWorkBudgeted reports caps that need a provider-enforced output bound.
func (a *Authority) UnboundedWorkBudgeted(route string) bool {
	return a.CostBudgetedForRoute(route) || a.Policy.TokensPerMinute != nil ||
		a.BudgetGroupID != nil && a.BudgetGroupTPM != nil ||
		a.Policy.RouteLimits[route].TokensPerMinute != nil ||
		a.Policy.EndUserPolicy.Limits(a.EndUserDigest).TokensPerMinute != nil ||
		a.ProjectEndUserPolicy.Limits(a.EndUserDigest).TokensPerMinute != nil
}
