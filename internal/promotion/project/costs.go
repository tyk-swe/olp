package project

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
)

type Costs interface {
	Install(ctx context.Context, snapshots []limits.Snapshot) error
}

func CostsOf(limiter *admission.Limiter) Costs {
	if limiter == nil {
		return noCosts{}
	}
	return limiterCosts{limiter: limiter}
}

type noCosts struct{}

func (noCosts) Install(context.Context, []limits.Snapshot) error { return nil }

type limiterCosts struct{ limiter *admission.Limiter }

func (c limiterCosts) Install(ctx context.Context, snapshots []limits.Snapshot) error {
	if err := c.limiter.InstallSnapshots(ctx, snapshots); err != nil {
		return access.Fail(http.StatusServiceUnavailable, "limits_unavailable", "Budget state is unavailable; retry the apply.")
	}
	return nil
}
