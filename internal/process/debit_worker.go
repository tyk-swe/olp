package process

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/usage"
)

func runDebitReconciliation(ctx context.Context, pool *pgxpool.Pool, limiter *limits.Limiter, log *slog.Logger) {
	store := &limits.DebitStore{Pool: pool, Limiter: limiter}
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		pass, cancel := context.WithTimeout(ctx, 45*time.Second)
		var err error
		if limiter == nil {
			_, err = store.RecoverPrepared(pass, 256)
		} else {
			err = store.ReconcileDebits(pass)
		}
		cancel()
		outcome := usage.OutcomeSuccess
		if err != nil {
			outcome = usage.OutcomeFailure
			log.Warn("execution debit reconciliation failed", "error", err)
		}
		checkpoint, checkpointCancel := context.WithTimeout(ctx, 5*time.Second)
		if checkpointErr := usage.CheckpointTask(checkpoint, pool, usage.TaskExecutionDebits, outcome, err == nil); checkpointErr != nil && ctx.Err() == nil {
			log.Warn("execution debit checkpoint failed", "error", checkpointErr)
		}
		checkpointCancel()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
