package ingest

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
	"github.com/tyk-swe/olp/internal/usage/completeness"
)

const consumerGroup = "olp:persistence"

const (
	// reclaimIdle is the pending-entry idle time after which another consumer
	// may claim the delivery from the one that went away.
	reclaimIdle = 30 * time.Second
	// batchSize bounds how many stream entries one read or reclaim returns.
	batchSize = 100
)

type Activity struct {
	Reclaimed  int64
	Recovered  int64
	Duplicates int64
	Processed  int64
}

const consumerCountersSQL = `UPDATE olp.async_worker_counters SET
        request_metadata_reclaimed_total = request_metadata_reclaimed_total + $1,
        request_metadata_recovered_total = request_metadata_recovered_total + $2,
        request_metadata_duplicates_total = request_metadata_duplicates_total + $3,
        request_metadata_processed_total = request_metadata_processed_total + $4
    WHERE singleton`

// ReportConsumerActivity folds one pass of the request metadata consumer into
// the installation counters and checkpoints the task, in a single transaction
// so the counters and the health row can never disagree about the same pass. A
// pass that did nothing writes nothing.
func ReportConsumerActivity(ctx context.Context, pool *pgxpool.Pool, region string, a Activity) error {
	if a.Reclaimed == 0 && a.Recovered == 0 && a.Duplicates == 0 && a.Processed == 0 {
		return nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("report consumer activity: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err = tx.Exec(ctx, consumerCountersSQL,
		a.Reclaimed, a.Recovered, a.Duplicates, a.Processed); err != nil {
		return fmt.Errorf("report consumer activity: %w", err)
	}
	progress := a.Reclaimed > 0 || a.Processed > 0
	if err = workerhealth.New(workerhealth.TaskRequestMetadataConsumer, region).Checkpoint(ctx, tx, workerhealth.OutcomeSuccess, progress); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("report consumer activity: %w", err)
	}
	return nil
}

// ConsumerName is this process's identity inside the consumer group. It carries
// the host and pid so an operator can find the owner of a pending delivery, and
// a per-process epoch so a restarted process never reclaims its own name (its
// abandoned entries stay pending for another consumer to claim).
func ConsumerName(hostname string) string {
	epoch := uuid.Must(uuid.NewV7())
	return completeness.ProcessName(hostname, os.Getpid(), epoch)
}
