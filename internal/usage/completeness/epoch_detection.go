package completeness

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
)

// Detection is what one stale-epoch pass found.
type Detection struct {
	CandidateEpochs          int64
	DetectedEpochs           int64
	UncertainEventLowerBound int64
}

const staleEpochsSQL = `SELECT gateway_instance, process_epoch::text, accepted, persisted, abandoned,
        updated_at, stale_candidate_at
    FROM olp.request_metadata_gateway_epochs
    WHERE gracefully_closed_at IS NULL AND stale_detected_at IS NULL AND updated_at < $1
    ORDER BY updated_at, gateway_instance, process_epoch
    LIMIT 100 FOR UPDATE SKIP LOCKED`

const markCandidateSQL = `UPDATE olp.request_metadata_gateway_epochs SET stale_candidate_at = $1
     WHERE gateway_instance = $2 AND process_epoch = $3
       AND gracefully_closed_at IS NULL AND stale_detected_at IS NULL`

// DetectStaleEpochs finds gateway epochs that stopped checkpointing and makes
// their loss durable. Detection takes two passes: the first marks a candidate,
// and only a later pass confirms it, so a process paused by a slow disk or a
// long stop-the-world is not declared lost while it is still alive — its next
// checkpoint clears the marker.
//
// `now` is a parameter so a caller can reason about both passes deterministically.
func DetectStaleEpochs(ctx context.Context, pool *pgxpool.Pool, now time.Time) (Detection, error) {
	staleCutoff := now.Add(-EpochStaleAfter)
	confirmCutoff := now.Add(-EpochConfirmAfter)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Detection{}, fmt.Errorf("detect stale gateway epochs: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, staleEpochsSQL, staleCutoff)
	if err != nil {
		return Detection{}, fmt.Errorf("detect stale gateway epochs: %w", err)
	}
	type candidate struct {
		epoch       unclean
		candidateAt *time.Time
	}
	var candidates []candidate
	for rows.Next() {
		var row candidate
		if err := rows.Scan(&row.epoch.gatewayInstance, &row.epoch.processEpoch,
			&row.epoch.accepted, &row.epoch.persisted, &row.epoch.abandoned,
			&row.epoch.lastCheckpoint, &row.candidateAt); err != nil {
			rows.Close()
			return Detection{}, fmt.Errorf("detect stale gateway epochs: %w", err)
		}
		row.epoch.detectedAt = now
		candidates = append(candidates, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Detection{}, fmt.Errorf("detect stale gateway epochs: %w", err)
	}
	var report Detection
	for _, row := range candidates {
		switch {
		case row.candidateAt == nil:
			if _, err := tx.Exec(ctx, markCandidateSQL, now, row.epoch.gatewayInstance,
				row.epoch.processEpoch); err != nil {
				return Detection{}, fmt.Errorf("detect stale gateway epochs: %w", err)
			}
			report.CandidateEpochs++
		case !row.candidateAt.After(confirmCutoff):
			lowerBound, err := recordUnclean(ctx, tx, row.epoch)
			if err != nil {
				return Detection{}, err
			}
			report.DetectedEpochs++
			report.UncertainEventLowerBound += lowerBound
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Detection{}, fmt.Errorf("detect stale gateway epochs: %w", err)
	}
	return report, nil
}

// RunEpochDetection confirms abandoned gateway epochs until ctx is done. It is
// safe to run on every worker: the scan skips locked rows and each epoch is
// recorded once.
func RunEpochDetection(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) {
	ticker := time.NewTicker(RecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			report, err := DetectStaleEpochs(ctx, pool, time.Now().UTC())
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Warn("stale gateway epoch detection failed", "error", err)
				if err := workerhealth.New(workerhealth.TaskEpochDetection, "").Checkpoint(ctx, pool, workerhealth.OutcomeFailure, false); err != nil {
					log.Warn("stale gateway epoch checkpoint failed", "error", err)
				}
				continue
			}
			progress := report.CandidateEpochs > 0 || report.DetectedEpochs > 0
			if err := workerhealth.New(workerhealth.TaskEpochDetection, "").Checkpoint(ctx, pool, workerhealth.OutcomeSuccess, progress); err != nil {
				log.Warn("stale gateway epoch checkpoint failed", "error", err)
			}
			if report.DetectedEpochs > 0 {
				log.Warn("unclean gateway shutdowns recorded as completeness gaps",
					"epochs", report.DetectedEpochs, "events", report.UncertainEventLowerBound)
			} else if report.CandidateEpochs > 0 {
				log.Warn("gateway epochs stopped checkpointing", "epochs", report.CandidateEpochs)
			}
		}
	}
}
