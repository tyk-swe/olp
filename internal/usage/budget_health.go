package usage

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrIncompleteBudgetAccounting = errors.New("budget accounting has lost events")

// Loss evidence has no reliable owner, so any gap overlapping an active budget
// window conservatively affects every cost budget. Use the accounting calendar:
// a week or a day during a zone transition can begin before the current month.
const budgetAccountingSQL = `WITH boundary AS MATERIALIZED (
	SELECT LEAST(daily_start, weekly_start, monthly_start) AS start_at FROM olp.budget_windows(now())
)
SELECT EXISTS (
	SELECT 1 FROM olp.request_metadata_ingestion_gaps, boundary WHERE last_observed_at >= start_at
) OR EXISTS (
	SELECT 1 FROM olp.request_metadata_gap_hourly, boundary WHERE last_observed_at >= start_at
) OR EXISTS (
	SELECT 1 FROM olp.request_metadata_gateway_epochs
	WHERE gracefully_closed_at IS NULL AND stale_detected_at IS NULL
	  AND updated_at < now() - make_interval(secs => $1)
) OR ($2::timestamptz IS NOT NULL AND $2 >= start_at) FROM boundary`

// CheckBudgetAccounting checks durable loss through retention and restarts,
// local loss before its checkpoint, and stale unclosed epochs even when no
// recovery worker is running. A healthy heartbeat can clear epoch uncertainty.
func CheckBudgetAccounting(ctx context.Context, pool *pgxpool.Pool, emitter *Emitter) error {
	var lastLoss *time.Time
	if emitter != nil {
		snapshot := emitter.Snapshot()
		if snapshot.Lost() > 0 {
			lastLoss = snapshot.LastLossAt
			if lastLoss == nil {
				return ErrIncompleteBudgetAccounting
			}
		}
	}
	if pool == nil {
		return ErrIncompleteBudgetAccounting
	}
	var incomplete bool
	if err := pool.QueryRow(ctx, budgetAccountingSQL, EpochStaleAfter.Seconds(), lastLoss).Scan(&incomplete); err != nil {
		return err
	}
	if incomplete {
		return ErrIncompleteBudgetAccounting
	}
	return nil
}

// RecordBudgetLoss makes dropped terminal events visible across replicas before
// the handler completes. Periodic epoch checkpoints recover a failed write.
func RecordBudgetLoss(ctx context.Context, pool *pgxpool.Pool, emitter *Emitter, instance string, counters *LossCounters) error {
	report, err := CheckpointEpoch(ctx, pool, instance, emitter.Snapshot(), false)
	if err == nil {
		recordLossCheckpoint(counters, report)
	}
	return err
}
