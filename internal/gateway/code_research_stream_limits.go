package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/routing"
)

// Notification streams have the configured concurrency bounds, but their own
// counters per service. They must not occupy tool-call slots. Request rates
// remain shared with ordinary traffic through the usual research reservation.
func (a *Admission) reserveResearchStream(ctx context.Context, authority keyauthority.Authority, route, service, provider string, providerLimits *routing.Limits) (*admission.Lease, *Error) {
	type boundary struct {
		scope    string
		max      *int64
		provider bool
	}
	boundaries := []boundary{
		{scope: "key:" + authority.LimitsLookup(), max: authority.Policy.MaxConcurrency},
		{scope: "route:" + authority.ID + ":" + route, max: authority.Policy.RouteLimits[route].MaxConcurrency},
		{scope: "key-end-user:" + authority.ID + ":" + authority.EndUserDigest, max: authority.Policy.EndUserPolicy.Limits(authority.EndUserDigest).MaxConcurrency},
	}
	if authority.BudgetGroupID != nil {
		boundaries = append(boundaries, boundary{scope: "group:" + *authority.BudgetGroupID, max: authority.BudgetGroupConcurrency})
	}
	if authority.ProjectID != nil {
		boundaries = append(boundaries, boundary{scope: "project-end-user:" + *authority.ProjectID + ":" + authority.EndUserDigest, max: authority.ProjectEndUserPolicy.Limits(authority.EndUserDigest).MaxConcurrency})
	}
	if providerLimits != nil {
		boundaries = append(boundaries, boundary{scope: "provider:" + provider, max: providerLimits.MaxConcurrency, provider: true})
	}
	var chain *admission.Lease
	for _, boundary := range boundaries {
		if boundary.max == nil {
			continue
		}
		refuse := func(failure *Error) (*admission.Lease, *Error) {
			settleKey(ctx, chain, false, nil, a.logger())
			return nil, failure
		}
		if !a.ready() {
			return refuse(limitsUnavailable())
		}
		digest := sha256.Sum256([]byte(service + "\x00" + boundary.scope))
		request := admission.Request{Lookup: "rs_" + hex.EncodeToString(digest[:16]), LeaseTTL: researchTimeout + reserveTimeout, Rate: admission.RateLimits{MaxConcurrency: boundary.max}}
		decision, cancel := context.WithTimeout(ctx, reserveTimeout)
		lease, err := a.limiter.Reserve(decision, request)
		cancel()
		if err != nil {
			if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
				return refuse(rateLimited(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate))
			}
			if boundary.provider {
				return refuse(limitsUnavailable())
			}
			if failure := a.outage(authority.ID, false, err); failure != nil {
				return refuse(failure)
			}
			continue
		}
		lease.Join(chain)
		chain = lease
	}
	return chain, nil
}
