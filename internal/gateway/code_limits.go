package gateway

import (
	"context"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
)

// ReserveCodeRate uses the shared request/token-rate/concurrency counters.
// Estimates here never become proven hard token bounds or subscription spend.
func (a *Admission) ReserveCodeRate(ctx context.Context, authority access.Authority, estimate int64, ttl time.Duration) (*limits.Lease, *Error) {
	authority.Policy.DailyCostLimit = nil
	authority.Policy.MonthlyCostLimit = nil
	authority.BudgetGroupID = nil
	return a.reserveKey(ctx, authority, estimate, ttl)
}
