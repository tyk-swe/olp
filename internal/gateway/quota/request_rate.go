package quota

import (
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
)

// RequestRateAuthority limits work that consumes requests without inference
// tokens. Control traffic also bypasses work concurrency.
func RequestRateAuthority(authority keyauthority.Authority, control bool) keyauthority.Authority {
	// This also clones the nested route and end-user policies before editing.
	authority = WithoutCostBudgets(authority)
	authority.Policy.TokensPerMinute = nil
	authority.BudgetGroupTPM = nil
	if control {
		authority.Policy.MaxConcurrency = nil
		authority.BudgetGroupConcurrency = nil
	}
	for slug, policy := range authority.Policy.RouteLimits {
		policy.TokensPerMinute = nil
		if control {
			policy.MaxConcurrency = nil
		}
		authority.Policy.RouteLimits[slug] = policy
	}
	for _, policy := range []*keyauthority.EndUserPolicy{authority.Policy.EndUserPolicy, authority.ProjectEndUserPolicy} {
		if policy != nil {
			policy.Defaults.TokensPerMinute = nil
			if control {
				policy.Defaults.MaxConcurrency = nil
			}
			for digest, limits := range policy.Overrides {
				limits.TokensPerMinute = nil
				if control {
					limits.MaxConcurrency = nil
				}
				policy.Overrides[digest] = limits
			}
		}
	}
	return authority
}
