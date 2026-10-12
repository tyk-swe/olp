package process

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

// Composition supervises the two feature-owned retention passes under the
// existing maintenance cadence and checkpoint. Neither feature owns the
// other's tables or lifecycle transactions.
func runMaintenance(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) {
	ticker := time.NewTicker(usage.MaintenanceInterval)
	defer ticker.Stop()
	store := resources.New(pool)
	for {
		now := time.Now()
		resourceCtx, stopResource := context.WithTimeout(ctx, usage.MaintenanceInterval/2)
		_, resourceErr := store.Expire(resourceCtx, now)
		stopResource()
		usageCtx, stopUsage := context.WithTimeout(ctx, usage.MaintenanceInterval/2)
		report, usageErr := usage.RunMaintenance(usageCtx, pool, now)
		stopUsage()
		if ctx.Err() != nil {
			return
		}
		outcome, progress := usage.OutcomeSuccess, report.LockAcquired
		if resourceErr != nil || usageErr != nil {
			outcome, progress = usage.OutcomeFailure, false
			log.Error("maintenance pass failed", "resource_error_type", fmt.Sprintf("%T", resourceErr), "usage_error_type", fmt.Sprintf("%T", usageErr))
		} else if !report.LockAcquired {
			outcome = usage.OutcomeSkipped
		}
		if err := usage.CheckpointTask(ctx, pool, usage.TaskMaintenance, outcome, progress); err != nil {
			log.Warn("maintenance health checkpoint failed", "error_type", fmt.Sprintf("%T", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
