package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
)

func (a *Admission) reserveRouteLimits(ctx context.Context, authority *access.Authority, route string, estimate int64, ttl time.Duration, hold costReservation, costChecked *bool) (*limits.Lease, *Error) {
	policy := authority.Policy.RouteLimits[route]
	if !policy.Limited() {
		return nil, nil
	}
	if policy.CostBudgeted() {
		if e := a.checkCostAccountingOnce(ctx, costChecked); e != nil {
			return nil, e
		}
	}
	if !a.ready() {
		return nil, limitsUnavailable()
	}
	if policy.TokensPerMinute != nil && estimate > *policy.TokensPerMinute {
		return nil, invalidRequest("request_exceeds_token_limit", "This request exceeds the API key token limit for this route.", nil)
	}
	owner := limits.KeyRouteBudgetID(authority.ID, route)
	request := limits.Request{
		CostOwnerID: owner, CostIncreases: authority.BudgetIncreases[owner], LookupID: limits.AggregateBudgetLookup(owner),
		RequestsPerMinute: policy.RequestsPerMinute, TokensPerMinute: policy.TokensPerMinute, MaxConcurrency: policy.MaxConcurrency,
		DailyCostLimit: policy.DailyCostLimit, MonthlyCostLimit: policy.MonthlyCostLimit, WeeklyCostLimit: policy.WeeklyCostLimit, RequestedTokens: estimate, LeaseTTL: ttl,
	}
	if policy.CostBudgeted() {
		request.CostEstimate, request.RequestID = hold.amount, hold.requestID
	}
	decision, cancel := context.WithTimeout(ctx, reserveTimeout)
	defer cancel()
	lease, err := a.limiter.Reserve(decision, request)
	if err == nil {
		return lease, nil
	}
	if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
		a.recordRejection(exceeded.Dimension)
		e := rateLimited(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate)
		param := "route_limits." + route
		e.Param = &param
		e.Message = "The API key limit for this route was exhausted."
		return nil, e
	}
	if e := a.outage(authority.ID, policy.CostBudgeted(), err); e != nil {
		return nil, e
	}
	return nil, nil
}

// limitRoute matches the route used by request accounting. Collection reads have
// no single route and remain subject to the other admission boundaries.
func (x *execution) limitRoute() string {
	if route := x.named(); route != nil {
		return route.Slug
	}
	return ""
}
