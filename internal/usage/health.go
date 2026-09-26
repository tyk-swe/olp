package usage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInvalidCheckpoint marks a health or epoch checkpoint whose counters
// cannot be true. Storing it would corrupt the evidence completeness is judged
// from, so the sample is refused and the caller retries with a fresh reading.
var ErrInvalidCheckpoint = errors.New("invalid request metadata checkpoint")

const reportConsumerHealthSQL = `INSERT INTO olp.request_metadata_consumer_health
        (singleton, pending_events, lag_events, oldest_pending_at, checked_at)
    VALUES (true, $1, $2, $3, $4)
    ON CONFLICT (singleton) DO UPDATE SET
        pending_events = EXCLUDED.pending_events,
        lag_events = EXCLUDED.lag_events,
        oldest_pending_at = EXCLUDED.oldest_pending_at,
        checked_at = EXCLUDED.checked_at
    WHERE request_metadata_consumer_health.checked_at <= EXCLUDED.checked_at`

// ReportConsumerHealth records one sample of the stream backlog: how many
// deliveries this group still owes, how far behind the tail it is, and how old
// the oldest outstanding delivery is. Usage completeness reads it, so a sample
// that contradicts itself (a backlog with no oldest delivery, or a reading from
// the future) is rejected rather than published.
//
// An older sample never overwrites a newer one: several consumers report the
// same group, and the freshest reading is the one that tells the truth.
func ReportConsumerHealth(ctx context.Context, pool *pgxpool.Pool, pending, lag int64,
	oldestPendingAt *time.Time, checkedAt time.Time) error {
	if pending < 0 || lag < 0 {
		return fmt.Errorf("%w: negative consumer backlog", ErrInvalidCheckpoint)
	}
	if (pending == 0) != (oldestPendingAt == nil) {
		return fmt.Errorf("%w: consumer backlog and oldest delivery disagree", ErrInvalidCheckpoint)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("report consumer health: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// The database clock is the one every reader compares against; a consumer
	// whose clock runs ahead must not park the health row in the future, where
	// no later sample could replace it.
	var databaseNow time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&databaseNow); err != nil {
		return fmt.Errorf("report consumer health: %w", err)
	}
	if checkedAt.After(databaseNow) {
		checkedAt = databaseNow
	}
	if oldestPendingAt != nil && oldestPendingAt.After(checkedAt.Add(5*time.Minute)) {
		return fmt.Errorf("%w: oldest delivery is after the sample", ErrInvalidCheckpoint)
	}
	if _, err = tx.Exec(ctx, reportConsumerHealthSQL, pending, lag, oldestPendingAt, checkedAt); err != nil {
		return fmt.Errorf("report consumer health: %w", err)
	}
	// Sampling the backlog is the consumer's proof of life. Progress is
	// reported separately by the pass that actually drained events.
	if err = CheckpointTask(ctx, tx, TaskRequestMetadataConsumer, OutcomeSuccess, false); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("report consumer health: %w", err)
	}
	return nil
}
