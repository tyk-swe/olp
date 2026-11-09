package project

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
)

type Costs interface {
	Install(ctx context.Context, snapshots []limits.CostSnapshot) error
}

func CostsOf(limiter *limits.Limiter) Costs {
	if limiter == nil {
		return noCosts{}
	}
	return limiterCosts{limiter: limiter}
}

type noCosts struct{}

func (noCosts) Install(context.Context, []limits.CostSnapshot) error { return nil }

type limiterCosts struct{ limiter *limits.Limiter }

func (c limiterCosts) Install(ctx context.Context, snapshots []limits.CostSnapshot) error {
	for _, snapshot := range snapshots {
		if _, _, err := c.limiter.ApplyCostSnapshot(ctx, snapshot); err != nil {
			return access.Fail(http.StatusServiceUnavailable, "limits_unavailable", "Budget state is unavailable; retry the apply.")
		}
	}
	return nil
}
