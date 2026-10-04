package usage

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrIncompleteBudgetAccounting = errors.New("budget accounting has lost events")

// CheckBudgetAccounting refuses to infer a spend total from incomplete facts.
// Gaps carry no key attribution, so every cost budget is conservatively affected
// until the current month has no overlapping loss. A local loss is checked even
// before its durable checkpoint reaches the other gateway replicas. Rolled-up
// gaps remain authoritative, and stale unclosed epochs fail closed even when
// no recovery worker is running to turn their uncertainty into dated gaps.
func CheckBudgetAccounting(ctx context.Context, pool *pgxpool.Pool, emitter *Emitter) error {
	if emitter != nil {
		snapshot := emitter.Snapshot()
		now := time.Now().UTC()
		month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		if snapshot.Lost() > 0 && (snapshot.LastLossAt == nil || !snapshot.LastLossAt.Before(month)) {
			return ErrIncompleteBudgetAccounting
		}
	}
	if pool == nil {
		return ErrIncompleteBudgetAccounting
	}
	var gap bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM olp.request_metadata_ingestion_gaps
		WHERE last_observed_at >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
	) OR EXISTS (
		SELECT 1 FROM olp.request_metadata_gap_hourly
		WHERE last_observed_at >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
	) OR EXISTS (
		SELECT 1 FROM olp.request_metadata_gateway_epochs
		WHERE gracefully_closed_at IS NULL AND stale_detected_at IS NULL
		  AND updated_at < now() - make_interval(secs => $1)
	)`, EpochStaleAfter.Seconds()).Scan(&gap)
	if err != nil {
		return err
	}
	if gap {
		return ErrIncompleteBudgetAccounting
	}
	return nil
}

// RecordBudgetLoss makes a rejected terminal event visible to budget admission
// on every replica. The emitter's usual epoch checkpoints remain the recovery
// path if the immediate database write is unavailable.
func RecordBudgetLoss(ctx context.Context, pool *pgxpool.Pool, emitter *Emitter, instance string, counters *LossCounters) error {
	report, err := CheckpointEpoch(ctx, pool, instance, emitter.Snapshot(), false)
	if err == nil {
		recordLossCheckpoint(counters, report)
	}
	return err
}
