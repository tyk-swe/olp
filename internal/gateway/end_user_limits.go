package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
)

// Each boundary reserves independently. A later refusal refunds every earlier
// reservation, while every dispatched request settles the complete chain.
func (a *Admission) reserveEndUsers(ctx context.Context, authority *access.Authority, estimate int64, ttl time.Duration, hold costReservation) (*limits.Lease, *Error) {
	if authority.Policy.EndUserPolicy == nil && authority.ProjectEndUserPolicy == nil {
		return nil, nil
	}
	if authority.EndUserDigest == "" {
		return nil, invalidRequest("invalid_end_user", "An end-user identity is required by policy.", nil)
	}
	if authority.Policy.EndUserPolicy.Blocks(authority.EndUserDigest) || authority.ProjectEndUserPolicy.Blocks(authority.EndUserDigest) {
		return nil, &Error{Status: http.StatusForbidden, Type: "permission_error", Code: "end_user_blocked", Message: "This end user is blocked by policy."}
	}
	var chain *limits.Lease
	for _, boundary := range [...]struct {
		policy  *access.EndUserPolicy
		project bool
	}{
		{authority.ProjectEndUserPolicy, true}, {authority.Policy.EndUserPolicy, false},
	} {
		policy := boundary.policy.Limits(authority.EndUserDigest)
		if !policy.Limited() {
			continue
		}
		owner := limits.EndUserKeyID(authority.ID, authority.EndUserDigest)
		if boundary.project {
			if authority.ProjectID == nil {
				settleKey(ctx, chain, false, nil, a.logger())
				return nil, limitsUnavailable()
			}
			owner = limits.EndUserProjectID(*authority.ProjectID, authority.EndUserDigest)
		}
		if !a.ready() {
			settleKey(ctx, chain, false, nil, a.logger())
			return nil, limitsUnavailable()
		}
		request := limits.Request{CostOwnerID: owner, CostIncreases: authority.BudgetIncreases[owner], LookupID: limits.EndUserLookup(owner),
			RequestsPerMinute: policy.RequestsPerMinute, TokensPerMinute: policy.TokensPerMinute,
			MaxConcurrency: policy.MaxConcurrency, DailyCostLimit: policy.DailyCostLimit, MonthlyCostLimit: policy.MonthlyCostLimit, WeeklyCostLimit: policy.WeeklyCostLimit,
			RequestedTokens: estimate, LeaseTTL: ttl}
		if policy.CostBudgeted() {
			request.CostEstimate, request.RequestID = hold.amount, hold.requestID
		}
		if policy.TokensPerMinute != nil && estimate > *policy.TokensPerMinute {
			settleKey(ctx, chain, false, nil, a.logger())
			return nil, invalidRequest("request_exceeds_token_limit", "This request exceeds the end-user token limit.", nil)
		}
		decision, cancel := context.WithTimeout(ctx, reserveTimeout)
		lease, err := a.limiter.Reserve(decision, request)
		cancel()
		if err == nil {
			lease.Attach(chain)
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
			return nil, budgetBoundaryError(rateLimited(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate), name, param)
		}
		if refusal := a.outage(authority.ID, policy.CostBudgeted(), err); refusal != nil {
			settleKey(ctx, chain, false, nil, a.logger())
			return nil, refusal
		}
	}
	return chain, nil
}
