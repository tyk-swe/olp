package gateway

import (
	"context"
	"errors"
	"maps"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/runtime"
)

// ReserveCodeRate uses the shared request/token-rate/concurrency counters.
// Estimates here never become proven hard token bounds or subscription spend.
func (a *Admission) ReserveCodeRate(ctx context.Context, authority access.Authority, estimate int64, ttl time.Duration, route ...string) (*limits.Lease, *Error) {
	authority = withoutCostBudgets(authority)
	// Code forwarding preserves upstream allowance headers rather than adding
	// the synthetic API-key allowance of the inference surfaces.
	return a.reserveKey(ctx, authority, "code", estimate, ttl, route...)
}

// withoutCostBudgets preserves rate, token, concurrency and identity controls.
func withoutCostBudgets(authority access.Authority) access.Authority {
	authority.Policy.WeeklyCostLimit = nil
	authority.Policy.DailyCostLimit = nil
	authority.Policy.MonthlyCostLimit = nil
	authority.BudgetGroupDailyCostLimit = nil
	authority.BudgetGroupMonthlyCostLimit = nil
	authority.BudgetGroupWeeklyCostLimit = nil
	authority.InstallationBudget, authority.ProjectBudget = nil, nil
	authority.ProjectAttributionBudgets = nil
	authority.Policy.RouteLimits = maps.Clone(authority.Policy.RouteLimits)
	for slug, policy := range authority.Policy.RouteLimits {
		policy.DailyCostLimit, policy.MonthlyCostLimit = nil, nil
		policy.WeeklyCostLimit = nil
		authority.Policy.RouteLimits[slug] = policy
	}
	authority.Policy.EndUserPolicy = codeEndUserPolicy(authority.Policy.EndUserPolicy)
	authority.ProjectEndUserPolicy = codeEndUserPolicy(authority.ProjectEndUserPolicy)
	authority.OrganizationBudget = nil
	return authority
}

// Subscription allowances have no invoice cost. Preserve every admission
// dimension that applies to code without mutating cached authority policy.
func codeEndUserPolicy(policy *access.EndUserPolicy) *access.EndUserPolicy {
	if policy == nil {
		return nil
	}
	copy := *policy
	copy.Defaults.DailyCostLimit, copy.Defaults.MonthlyCostLimit = nil, nil
	copy.Defaults.WeeklyCostLimit = nil
	copy.Overrides = maps.Clone(policy.Overrides)
	for digest, limits := range copy.Overrides {
		limits.DailyCostLimit, limits.MonthlyCostLimit = nil, nil
		limits.WeeklyCostLimit = nil
		copy.Overrides[digest] = limits
	}
	return &copy
}

func (a *Admission) reserveCodeProvider(ctx context.Context, providerID string, configuration runtime.Configuration, estimate int64, ttl time.Duration) (*limits.Lease, *Error) {
	request := connectionRequest(&runtime.Provider{ID: providerID, Limits: configuration.Options.Limits}, estimate, ttl, "")
	if !request.HasHardLimits() {
		return nil, nil
	}
	if !a.ready() {
		return nil, limitsUnavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, reserveTimeout)
	defer cancel()
	lease, err := a.limiter.Reserve(ctx, request)
	if err == nil {
		return lease, nil
	}
	if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
		return nil, rateLimited(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate)
	}
	return nil, limitsUnavailable()
}
