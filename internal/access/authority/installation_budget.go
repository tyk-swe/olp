package authority

func NormalizedBudget(policy *BudgetPolicy) *BudgetPolicy {
	if !policy.Limited() {
		return nil
	}
	return policy
}
