package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/runtime"
)

// ReserveCodeRate uses the shared request/token-rate/concurrency counters.
// Estimates here never become proven hard token bounds or subscription spend.
func (a *Admission) ReserveCodeRate(ctx context.Context, authority access.Authority, estimate int64, ttl time.Duration) (*limits.Lease, *Error) {
	authority.Policy.DailyCostLimit = nil
	authority.Policy.MonthlyCostLimit = nil
	authority.BudgetGroupID = nil
	// Code forwarding preserves upstream allowance headers rather than adding
	// the synthetic API-key allowance of the inference surfaces.
	return a.reserveKey(ctx, authority, "code", estimate, ttl)
}

func (a *Admission) reserveCodeProvider(ctx context.Context, providerID string, configuration runtime.Configuration, estimate int64, ttl time.Duration) (*limits.Lease, *Error) {
	request := connectionRequest(&runtime.Provider{ID: providerID, Limits: configuration.Options.Limits}, estimate, ttl)
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
