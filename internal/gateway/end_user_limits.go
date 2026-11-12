package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
)

// Each boundary reserves independently. A later refusal refunds every earlier
// reservation, while every dispatched request settles the complete chain.
func (a *Admission) reserveEndUsers(ctx context.Context, authority *keyauthority.Authority, estimate int64, ttl time.Duration, hold costReservation, costChecked *bool) (*admission.Lease, *fault.Error) {
	if authority.Policy.EndUserPolicy == nil && authority.ProjectEndUserPolicy == nil {
		return nil, nil
	}
	if authority.EndUserDigest == "" {
		return nil, fault.InvalidRequest("invalid_end_user", "An end-user identity is required by policy.", nil)
	}
	if authority.Policy.EndUserPolicy.Blocks(authority.EndUserDigest) || authority.ProjectEndUserPolicy.Blocks(authority.EndUserDigest) {
		return nil, &fault.Error{Status: http.StatusForbidden, Type: "permission_error", Code: "end_user_blocked", Message: "This end user is blocked by policy."}
	}
	var chain *admission.Lease
	for _, boundary := range [...]struct {
		policy  *keyauthority.EndUserPolicy
		project bool
	}{
		{authority.ProjectEndUserPolicy, true}, {authority.Policy.EndUserPolicy, false},
	} {
		policy := boundary.policy.Limits(authority.EndUserDigest)
		if !policy.Limited() {
			continue
		}
		if policy.CostBudgeted() {
			if e := a.checkCostAccountingOnce(ctx, costChecked); e != nil {
				settleKey(ctx, chain, false, nil, a.logger())
				return nil, e
			}
		}
		owner := limits.EndUserKeyID(authority.ID, authority.EndUserDigest)
		if boundary.project {
			if authority.ProjectID == nil {
				settleKey(ctx, chain, false, nil, a.logger())
				return nil, limitsUnavailable()
			}
			owner = limits.EndUserProjectID(*authority.ProjectID, authority.EndUserDigest)
		}
		if policy.CostBudgeted() {
			if e := a.checkPendingCostAccounting(ctx, owner); e != nil {
				settleKey(ctx, chain, false, nil, a.logger())
				return nil, e
			}
		}
		if policy.TokensPerMinute != nil {
			if e := a.checkPendingTokenAccounting(ctx, owner); e != nil {
				settleKey(ctx, chain, false, nil, a.logger())
				return nil, e
			}
		}
		if !a.ready() {
			settleKey(ctx, chain, false, nil, a.logger())
			return nil, limitsUnavailable()
		}
		request := hold.on(admission.Request{
			Lookup: limits.EndUserLookup(owner), LeaseTTL: ttl,
			Rate: admission.RateLimits{RequestsPerMinute: policy.RequestsPerMinute, TokensPerMinute: policy.TokensPerMinute, MaxConcurrency: policy.MaxConcurrency, Tokens: estimate},
			Cost: admission.Budget(limits.Owner{ID: owner, Kind: limits.OwnerEndUser, Basis: limits.Basis(policy.CostBasis)},
				policy.DailyCostLimit, policy.WeeklyCostLimit, policy.MonthlyCostLimit, authority.BudgetIncreases[owner]),
		})
		if policy.TokensPerMinute != nil && estimate > *policy.TokensPerMinute {
			settleKey(ctx, chain, false, nil, a.logger())
			return nil, fault.InvalidRequest("request_exceeds_token_limit", "This request exceeds the end-user token limit.", nil)
		}
		decision, cancel := context.WithTimeout(ctx, reserveTimeout)
		lease, err := a.limiter.Reserve(decision, request)
		cancel()
		if err == nil {
			lease.Join(chain)
			chain = lease
			continue
		}
		if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
			a.recordRejection(exceeded.Dimension)
			settleKey(ctx, chain, false, nil, a.logger())
			name, param := "key end-user", "end_user.key"
			if boundary.project {
				name, param = "project end-user", "end_user.project"
			}
			return nil, budgetBoundaryError(rateLimitedBasis(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate, exceeded.Basis), name, param)
		}
		if refusal := a.outage(authority.ID, policy.CostBudgeted(), err); refusal != nil {
			settleKey(ctx, chain, false, nil, a.logger())
			return nil, refusal
		}
	}
	return chain, nil
}
