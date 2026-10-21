package process

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

// Composition supervises resource, batch-settlement and usage retention under
// the existing maintenance cadence and checkpoint.
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
		var batchErr error
		if report.LockAcquired {
			batchStore := batches.NewStore(pool)
			_, batchErr = batchStore.PurgeClosedBefore(usageCtx, report.HistoryCutoff)
		}
		stopUsage()
		if ctx.Err() != nil {
			return
		}
		outcome, progress := usage.OutcomeSuccess, report.LockAcquired
		if resourceErr != nil || usageErr != nil || batchErr != nil {
			outcome, progress = usage.OutcomeFailure, false
			log.Error("maintenance pass failed", "resource_error_type", fmt.Sprintf("%T", resourceErr), "usage_error_type", fmt.Sprintf("%T", usageErr), "batch_error_type", fmt.Sprintf("%T", batchErr))
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
