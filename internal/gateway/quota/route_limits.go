package quota

import (
	"context"
	"errors"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
)

func (a *Admission) ReserveRouteLimits(ctx context.Context, authority *keyauthority.Authority, route string, estimate int64, ttl time.Duration, hold CostReservation, costChecked *bool) (*admission.Lease, *fault.Error) {
	policy := authority.Policy.RouteLimits[route]
	if !policy.Limited() {
		return nil, nil
	}
	owner := limits.KeyRouteBudgetID(authority.ID, route)
	basis := limits.Owner{ID: owner, Kind: limits.OwnerAggregate, Basis: limits.Basis(policy.CostBasis)}
	request := hold.On(admission.Request{
		Lookup: limits.AggregateBudgetLookup(owner), LeaseTTL: ttl,
		Rate: admission.RateLimits{RequestsPerMinute: policy.RequestsPerMinute, TokensPerMinute: policy.TokensPerMinute, MaxConcurrency: policy.MaxConcurrency, Tokens: estimate},
		Cost: admission.Budget(basis, policy.DailyCostLimit, policy.WeeklyCostLimit, policy.MonthlyCostLimit, authority.BudgetIncreases[owner]),
	})
	if policy.TokensPerMinute != nil {
		if e := a.checkPendingTokenAccounting(ctx, owner); e != nil {
			return nil, e
		}
	}
	if policy.CostBudgeted() {
		if e := a.checkCostAccountingOnce(ctx, costChecked); e != nil {
			return nil, e
		}
		if e := a.CheckPendingCostAccounting(ctx, owner); e != nil {
			return nil, e
		}
	}
	if !a.Ready() {
		return nil, LimitsUnavailable()
	}
	if policy.TokensPerMinute != nil && estimate > *policy.TokensPerMinute {
		return nil, fault.InvalidRequest("request_exceeds_token_limit", "This request exceeds the API key token limit for this route.", nil)
	}
	decision, cancel := context.WithTimeout(ctx, ReserveTimeout)
	defer cancel()
	lease, err := a.limiter.Reserve(decision, request)
	if err == nil {
		return lease, nil
	}
	if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
		a.RecordRejection(exceeded.Dimension)
		e := RateLimitedBasis(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate, exceeded.Basis)
		param := "route_limits." + route
		e.Param = &param
		e.Message = "The API key limit for this route was exhausted."
		return nil, e
	}
	if e := a.Outage(authority.ID, policy.CostBudgeted(), err); e != nil {
		return nil, e
	}
	return nil, nil
}
