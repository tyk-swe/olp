package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/routing"
)

// Notification streams have the configured concurrency bounds, but their own
// counters per service. They must not occupy tool-call slots. Request rates
// remain shared with ordinary traffic through the usual research reservation.
func (a *Admission) reserveResearchStream(ctx context.Context, authority access.Authority, route, service, provider string, providerLimits *routing.Limits) (*limits.Lease, *Error) {
	type boundary struct {
		scope string
		max   *int64
	}
	boundaries := []boundary{
		{"key:" + authority.LimitsLookup(), authority.Policy.MaxConcurrency},
		{"route:" + authority.ID + ":" + route, authority.Policy.RouteLimits[route].MaxConcurrency},
		{"key-end-user:" + authority.ID + ":" + authority.EndUserDigest, authority.Policy.EndUserPolicy.Limits(authority.EndUserDigest).MaxConcurrency},
	}
	if authority.BudgetGroupID != nil {
		boundaries = append(boundaries, boundary{"group:" + *authority.BudgetGroupID, authority.BudgetGroupConcurrency})
	}
	if authority.ProjectID != nil {
		boundaries = append(boundaries, boundary{"project-end-user:" + *authority.ProjectID + ":" + authority.EndUserDigest, authority.ProjectEndUserPolicy.Limits(authority.EndUserDigest).MaxConcurrency})
	}
	if providerLimits != nil {
		boundaries = append(boundaries, boundary{"provider:" + provider, providerLimits.MaxConcurrency})
	}
	var chain *limits.Lease
	for _, boundary := range boundaries {
		if boundary.max == nil {
			continue
		}
		refuse := func(failure *Error) (*limits.Lease, *Error) {
			settleKey(ctx, chain, false, nil, a.logger())
			return nil, failure
		}
		if !a.ready() {
			return refuse(limitsUnavailable())
		}
		digest := sha256.Sum256([]byte(service + "\x00" + boundary.scope))
		request := limits.Request{CostOwnerID: authority.ID, LookupID: "rs_" + hex.EncodeToString(digest[:16]), MaxConcurrency: boundary.max, LeaseTTL: researchTimeout + reserveTimeout}
		decision, cancel := context.WithTimeout(ctx, reserveTimeout)
		lease, err := a.limiter.Reserve(decision, request)
		cancel()
		if err != nil {
			if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
				return refuse(rateLimited(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate))
			}
			return refuse(limitsUnavailable())
		}
		lease.Attach(chain)
		chain = lease
	}
	return chain, nil
}
