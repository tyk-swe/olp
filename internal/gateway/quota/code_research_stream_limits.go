package quota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/routing"
)

// Notification streams have the configured concurrency bounds, but their own
// counters per service. They must not occupy tool-call slots. Request rates
// remain shared with ordinary traffic through the usual research reservation.
func (a *Admission) ReserveResearchStream(ctx context.Context, authority keyauthority.Authority, route, service, provider string, providerLimits *routing.Limits, hold time.Duration) (*admission.Lease, *fault.Error) {
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
		refuse := func(failure *fault.Error) (*admission.Lease, *fault.Error) {
			SettleKey(ctx, chain, false, nil, a.Logger())
			return nil, failure
		}
		if !a.Ready() {
			return refuse(LimitsUnavailable())
		}
		digest := sha256.Sum256([]byte(service + "\x00" + boundary.scope))
		request := admission.Request{Lookup: "rs_" + hex.EncodeToString(digest[:16]), LeaseTTL: hold + ReserveTimeout, Rate: admission.RateLimits{MaxConcurrency: boundary.max}}
		decision, cancel := context.WithTimeout(ctx, ReserveTimeout)
		lease, err := a.limiter.Reserve(decision, request)
		cancel()
		if err != nil {
			if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
				return refuse(RateLimited(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate))
			}
			if boundary.provider {
				return refuse(LimitsUnavailable())
			}
			if failure := a.Outage(authority.ID, false, err); failure != nil {
				return refuse(failure)
			}
			continue
		}
		lease.Join(chain)
		chain = lease
	}
	return chain, nil
}
