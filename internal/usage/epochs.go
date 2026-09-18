package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// epochLockSeed serializes the checkpoints of one gateway instance. Two
// processes reporting the same label must not interleave their reads and
// writes, or one would supersede the other's epoch on stale counters.
const epochLockSeed = int64(0x4f4c505f5545)

// lossReporterInterval paces durable loss reporting. The gateway never waits
// for it: a request is served, its event is buffered, and this loop is what
// later admits whatever the buffer could not deliver.
const lossReporterInterval = time.Second

// uuid7 mints a time ordered identifier for a row this package inserts.
func uuid7() string { return uuid.Must(uuid.NewV7()).String() }

// ErrInvalidGap marks a gap that cannot be recorded as described.
var ErrInvalidGap = errors.New("invalid request metadata gap")

// Gap is a bounded admission that some request metadata never became usage.
// Reports read gaps as evidence that a range is incomplete, so the window is
// as narrow as the evidence allows.
type Gap struct {
	GatewayInstance string
	EventCount      int64
	Reason          string
	FirstObservedAt time.Time
	LastObservedAt  time.Time
}

const insertGapSQL = `INSERT INTO olp_go.request_metadata_ingestion_gaps
        (id, gateway_instance, event_count, reason, first_observed_at, last_observed_at,
         deduplication_key)
    VALUES ($1, $2, $3, $4, $5, $6, $7)
    ON CONFLICT (deduplication_key) WHERE deduplication_key IS NOT NULL DO NOTHING`

// ReportGapOnce records a gap at most once for the given deduplication key and
// reports whether this call is the one that recorded it. The key makes the
// report idempotent: the same lost delivery, replayed by another consumer or
// after a restart, must widen no total twice.
func ReportGapOnce(ctx context.Context, pool *pgxpool.Pool, g Gap, dedupeKey string) (bool, error) {
	if strings.TrimSpace(dedupeKey) == "" || len(dedupeKey) > 256 {
		return false, fmt.Errorf("%w: deduplication key", ErrInvalidGap)
	}
	return insertGap(ctx, pool, g, &dedupeKey)
}

func insertGap(ctx context.Context, q Execer, g Gap, dedupeKey *string) (bool, error) {
	if g.EventCount <= 0 || strings.TrimSpace(g.GatewayInstance) == "" ||
		strings.TrimSpace(g.Reason) == "" || g.LastObservedAt.Before(g.FirstObservedAt) {
		return false, fmt.Errorf("%w: %s", ErrInvalidGap, g.Reason)
	}
	tag, err := q.Exec(ctx, insertGapSQL, uuid7(), g.GatewayInstance,
		g.EventCount, g.Reason, g.FirstObservedAt, g.LastObservedAt, dedupeKey)
	if err != nil {
		return false, fmt.Errorf("record request metadata gap: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// LossReport is what one epoch checkpoint made durable.
type LossReport struct {
	ReportedEvents      int64
	ReportedDropped     int64
	ReportedAbandoned   int64
	ProcessEpochChanged bool
}

// LossCounters are the process-wide totals of request-metadata loss the
// reporter has durably recorded in PostgreSQL. The loss reporter owns the
// increments and the metrics endpoint renders them.
type LossCounters struct {
	events    atomic.Int64
	dropped   atomic.Int64
	abandoned atomic.Int64
}

// Record folds one durable checkpoint into the process totals.
func (c *LossCounters) Record(report LossReport) {
	c.events.Add(report.ReportedEvents)
	c.dropped.Add(report.ReportedDropped)
	c.abandoned.Add(report.ReportedAbandoned)
}

// Totals is the durable loss this process has reported so far.
func (c *LossCounters) Totals() (events, dropped, abandoned int64) {
	return c.events.Load(), c.dropped.Load(), c.abandoned.Load()
}

const uncleanGapSQL = `INSERT INTO olp_go.request_metadata_ingestion_gaps
        (id, gateway_instance, event_count, reason, certainty, first_observed_at,
         last_observed_at, reported_at)
    VALUES ($1, $2, $3, 'gateway_epoch_unclean_shutdown', 'lower_bound', $4, $5, $5)`

const markUncleanSQL = `UPDATE olp_go.request_metadata_gateway_epochs
       SET stale_detected_at = $1, uncertainty_gap_id = $2
     WHERE gateway_instance = $3 AND process_epoch = $4
       AND gracefully_closed_at IS NULL AND stale_detected_at IS NULL`

// unclean is an epoch that stopped checkpointing without closing.
type unclean struct {
	gatewayInstance string
	processEpoch    string
	accepted        int64
	persisted       int64
	abandoned       int64
	lastCheckpoint  time.Time
	detectedAt      time.Time
}

// recordUnclean marks one epoch as lost and attaches the gap that bounds how
// much it may have taken with it. The count is a lower bound, not a guess: only
// the events this epoch accepted and never accounted for are claimed.
func recordUnclean(ctx context.Context, tx pgx.Tx, epoch unclean) (int64, error) {
	lowerBound := max(epoch.accepted-epoch.persisted-epoch.abandoned, 0)
	gapID := uuid7()
	lastObserved := epoch.detectedAt
	if epoch.lastCheckpoint.After(lastObserved) {
		lastObserved = epoch.lastCheckpoint
	}
	if _, err := tx.Exec(ctx, uncleanGapSQL, gapID, epoch.gatewayInstance, lowerBound,
		epoch.lastCheckpoint, lastObserved); err != nil {
		return 0, fmt.Errorf("record unclean gateway epoch: %w", err)
	}
	if _, err := tx.Exec(ctx, markUncleanSQL, epoch.detectedAt, gapID,
		epoch.gatewayInstance, epoch.processEpoch); err != nil {
		return 0, fmt.Errorf("record unclean gateway epoch: %w", err)
	}
	return lowerBound, nil
}

// validateCheckpoint narrows a snapshot to what an epoch row may hold. The
// label is stored and displayed, and the counters must be internally
// consistent, because the row is later read as proof of what was delivered.
func validateCheckpoint(gatewayInstance string, s Snapshot, graceful bool) (string, string, error) {
	label := strings.TrimSpace(gatewayInstance)
	hasControl := strings.ContainsFunc(label, unicode.IsControl)
	switch {
	case label == "" || len(label) > 200 || hasControl:
		return "", "", fmt.Errorf("%w: gateway instance label", ErrInvalidCheckpoint)
	case s.Accepted < 0 || s.Persisted < 0 || s.Dropped < 0 || s.Abandoned < 0:
		return "", "", fmt.Errorf("%w: negative counters", ErrInvalidCheckpoint)
	case s.Persisted > s.Accepted || s.Abandoned > s.Accepted-s.Persisted:
		return "", "", fmt.Errorf("%w: counters exceed accepted events", ErrInvalidCheckpoint)
	case graceful && !s.GracefullyDrained():
		return "", "", fmt.Errorf("%w: writer has not drained", ErrInvalidCheckpoint)
	}
	epoch, err := uuid.Parse(s.ProcessEpoch)
	if err != nil {
		return "", "", fmt.Errorf("%w: process epoch", ErrInvalidCheckpoint)
	}
	return label, epoch.String(), nil
}

const baselineSQL = `SELECT accepted, persisted, dropped, abandoned, writer_closed, updated_at,
        gracefully_closed_at, stale_detected_at
    FROM olp_go.request_metadata_gateway_epochs
    WHERE gateway_instance = $1 AND process_epoch = $2 FOR UPDATE`

const supersededSQL = `SELECT process_epoch::text, accepted, persisted, abandoned, updated_at
    FROM olp_go.request_metadata_gateway_epochs
    WHERE gateway_instance = $1 AND process_epoch <> $2
      AND gracefully_closed_at IS NULL AND stale_detected_at IS NULL FOR UPDATE`

// baseline is the previous checkpoint this one is measured against.
type baseline struct {
	dropped    int64
	abandoned  int64
	checkpoint time.Time
}

// loadBaseline locks this epoch's row and returns what the last checkpoint saw.
// A missing row means this process is new: every other open epoch of the same
// gateway instance belonged to a process that is gone, and is closed as
// unclean here rather than waiting for the detector, because the restart is
// already proof that it stopped.
func loadBaseline(ctx context.Context, tx pgx.Tx, instance, processEpoch string,
	s Snapshot, graceful bool, now time.Time) (bool, *baseline, error) {
	var previous struct {
		accepted, persisted, dropped, abandoned int64
		writerClosed                            bool
		updatedAt                               time.Time
		gracefullyClosedAt, staleDetectedAt     *time.Time
	}
	err := tx.QueryRow(ctx, baselineSQL, instance, processEpoch).Scan(&previous.accepted,
		&previous.persisted, &previous.dropped, &previous.abandoned, &previous.writerClosed,
		&previous.updatedAt, &previous.gracefullyClosedAt, &previous.staleDetectedAt)
	missing := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !missing {
		return false, nil, fmt.Errorf("load gateway epoch checkpoint: %w", err)
	}
	if missing {
		superseded, err := supersededEpochs(ctx, tx, instance, processEpoch)
		if err != nil {
			return false, nil, err
		}
		for _, epoch := range superseded {
			epoch.gatewayInstance, epoch.detectedAt = instance, now
			if _, err := recordUnclean(ctx, tx, epoch); err != nil {
				return false, nil, err
			}
		}
		return true, &baseline{checkpoint: s.StartedAt}, nil
	}
	switch {
	case previous.staleDetectedAt != nil:
		// The epoch was already declared lost and its gap recorded. Reviving it
		// would contradict evidence an operator may already have acted on.
		return false, nil, fmt.Errorf("%w: epoch was detected as stale", ErrInvalidCheckpoint)
	case s.Accepted < previous.accepted || s.Persisted < previous.persisted ||
		s.Dropped < previous.dropped || s.Abandoned < previous.abandoned ||
		(previous.writerClosed && !s.Closed):
		return false, nil, fmt.Errorf("%w: counters regressed", ErrInvalidCheckpoint)
	case previous.gracefullyClosedAt != nil:
		identical := graceful && s.Closed && s.Accepted == previous.accepted &&
			s.Persisted == previous.persisted && s.Dropped == previous.dropped &&
			s.Abandoned == previous.abandoned
		if identical {
			// A repeated close of an already closed epoch: nothing changed, so
			// nothing is written and no loss is double counted.
			return false, nil, nil
		}
		return false, nil, fmt.Errorf("%w: epoch was already closed", ErrInvalidCheckpoint)
	}
	return false, &baseline{previous.dropped, previous.abandoned, previous.updatedAt}, nil
}

func supersededEpochs(ctx context.Context, tx pgx.Tx, instance, processEpoch string) ([]unclean, error) {
	rows, err := tx.Query(ctx, supersededSQL, instance, processEpoch)
	if err != nil {
		return nil, fmt.Errorf("load superseded gateway epochs: %w", err)
	}
	defer rows.Close()
	var superseded []unclean
	for rows.Next() {
		var epoch unclean
		if err := rows.Scan(&epoch.processEpoch, &epoch.accepted, &epoch.persisted,
			&epoch.abandoned, &epoch.lastCheckpoint); err != nil {
			return nil, fmt.Errorf("load superseded gateway epochs: %w", err)
		}
		superseded = append(superseded, epoch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load superseded gateway epochs: %w", err)
	}
	return superseded, nil
}

const bufferLossGapSQL = `INSERT INTO olp_go.request_metadata_ingestion_gaps
        (id, gateway_instance, event_count, reason, first_observed_at, last_observed_at, reported_at)
    VALUES ($1, $2, $3, 'gateway_local_buffer_loss', $4, $5, $6)`

const upsertEpochSQL = `INSERT INTO olp_go.request_metadata_gateway_epochs
        (gateway_instance, process_epoch, started_at, accepted, persisted, dropped, abandoned,
         retrying, writer_closed, updated_at, gracefully_closed_at)
    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
            CASE WHEN $11 THEN $10::timestamptz ELSE NULL END)
    ON CONFLICT (gateway_instance, process_epoch) DO UPDATE SET
        accepted = EXCLUDED.accepted, persisted = EXCLUDED.persisted,
        dropped = EXCLUDED.dropped, abandoned = EXCLUDED.abandoned,
        retrying = EXCLUDED.retrying, writer_closed = EXCLUDED.writer_closed,
        updated_at = EXCLUDED.updated_at, stale_candidate_at = NULL,
        gracefully_closed_at = CASE WHEN $11
            THEN COALESCE(request_metadata_gateway_epochs.gracefully_closed_at, $10::timestamptz)
            ELSE request_metadata_gateway_epochs.gracefully_closed_at END`

// CheckpointEpoch makes this process's delivery accounting durable and records
// whatever the buffer lost since the last checkpoint as an exact gap. Passing
// `graceful` closes the epoch, which is only allowed once the writer has
// stopped with nothing outstanding: a clean close is a promise that nothing was
// left behind, and the stale detector never revisits it.
func CheckpointEpoch(ctx context.Context, pool *pgxpool.Pool, gatewayInstance string,
	s Snapshot, graceful bool) (LossReport, error) {
	instance, processEpoch, err := validateCheckpoint(gatewayInstance, s, graceful)
	if err != nil {
		return LossReport{}, err
	}
	now := time.Now().UTC()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return LossReport{}, fmt.Errorf("checkpoint gateway epoch: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, $2))",
		instance, epochLockSeed); err != nil {
		return LossReport{}, fmt.Errorf("checkpoint gateway epoch: %w", err)
	}
	changed, previous, err := loadBaseline(ctx, tx, instance, processEpoch, s, graceful, now)
	if err != nil {
		return LossReport{}, err
	}
	if previous == nil {
		// An identical re-close. The superseded-epoch bookkeeping above may
		// still have work to commit.
		if err = tx.Commit(ctx); err != nil {
			return LossReport{}, fmt.Errorf("checkpoint gateway epoch: %w", err)
		}
		return LossReport{}, nil
	}
	droppedDelta := s.Dropped - previous.dropped
	abandonedDelta := s.Abandoned - previous.abandoned
	events := droppedDelta + abandonedDelta
	if events > 0 {
		last := now
		if s.LastLossAt != nil {
			last = *s.LastLossAt
		}
		first := lossWindowStart(s, changed, previous.checkpoint)
		if first.After(last) {
			first = last
		}
		if _, err = tx.Exec(ctx, bufferLossGapSQL, uuid7(), instance,
			events, first, last, now); err != nil {
			return LossReport{}, fmt.Errorf("record gateway buffer loss: %w", err)
		}
	}
	if _, err = tx.Exec(ctx, upsertEpochSQL, instance, processEpoch, s.StartedAt, s.Accepted,
		s.Persisted, s.Dropped, s.Abandoned, s.Retrying, s.Closed, now, graceful); err != nil {
		return LossReport{}, fmt.Errorf("checkpoint gateway epoch: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return LossReport{}, fmt.Errorf("checkpoint gateway epoch: %w", err)
	}
	return LossReport{
		ReportedEvents:      max(events, 0),
		ReportedDropped:     max(droppedDelta, 0),
		ReportedAbandoned:   max(abandonedDelta, 0),
		ProcessEpochChanged: changed,
	}, nil
}

// lossWindowStart bounds when the reported loss can have begun. Within one
// epoch the window opens no earlier than the previous checkpoint, because
// everything before it was already reported; across a process restart the
// epoch's own start is the earliest it could have happened.
func lossWindowStart(s Snapshot, processEpochChanged bool, previousCheckpoint time.Time) time.Time {
	if processEpochChanged {
		if s.FirstLossAt != nil {
			return *s.FirstLossAt
		}
		return s.StartedAt
	}
	if s.FirstLossAt != nil && s.FirstLossAt.After(previousCheckpoint) {
		return *s.FirstLossAt
	}
	return previousCheckpoint
}

// RunLossReporter checkpoints the emitter until ctx is done, then closes the
// epoch. The close is retried for a few seconds with a context that outlives
// the shutdown, because an epoch left open is later reported as an unclean
// shutdown by the detector, and a process that shut down cleanly should not
// leave that scar.
func RunLossReporter(ctx context.Context, pool *pgxpool.Pool, emitter *Emitter,
	gatewayInstance string, counters *LossCounters, log *slog.Logger) {
	ticker := time.NewTicker(lossReporterInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			closeEpoch(ctx, pool, emitter, gatewayInstance, counters, log)
			return
		case <-ticker.C:
			report, err := CheckpointEpoch(ctx, pool, gatewayInstance, emitter.Snapshot(), false)
			if err != nil {
				if ctx.Err() == nil {
					log.Warn("request metadata loss could not be reported", "error", err)
				}
				continue
			}
			recordLossCheckpoint(counters, report)
			logLossReport(log, report)
		}
	}
}

// recordLossCheckpoint folds a successful checkpoint into the process's
// durable-loss counters, which the metrics endpoint renders.
func recordLossCheckpoint(counters *LossCounters, report LossReport) {
	if counters != nil {
		counters.Record(report)
	}
}

func closeEpoch(ctx context.Context, pool *pgxpool.Pool, emitter *Emitter,
	gatewayInstance string, counters *LossCounters, log *slog.Logger) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Second)
	defer cancel()
	for {
		snapshot := emitter.Snapshot()
		// Forced HTTP shutdown may leave emitters in flight. Record final
		// counters, but leave the epoch for detection rather than claiming a
		// clean close whose last events have not yet been observed.
		report, err := CheckpointEpoch(cleanup, pool, gatewayInstance, snapshot, !snapshot.Unclean)
		if err == nil {
			recordLossCheckpoint(counters, report)
			logLossReport(log, report)
			return
		}
		if cleanup.Err() != nil {
			log.Error("request metadata epoch could not be closed",
				"error", err, "lost", snapshot.Lost())
			return
		}
		log.Warn("request metadata epoch close failed; retrying", "error", err)
		if waitForRetry(cleanup, 200*time.Millisecond) {
			log.Error("request metadata epoch could not be closed", "error", cleanup.Err())
			return
		}
	}
}

// logLossReport reports loss ahead of an epoch change: a restart is routine,
// losing events is not.
func logLossReport(log *slog.Logger, report LossReport) {
	switch {
	case report.ReportedEvents > 0:
		log.Warn("request metadata loss durably reported",
			"events", report.ReportedEvents, "dropped", report.ReportedDropped,
			"abandoned", report.ReportedAbandoned)
	case report.ProcessEpochChanged:
		log.Info("request metadata gateway epoch started")
	}
}

// Detection is what one stale-epoch pass found.
type Detection struct {
	CandidateEpochs          int64
	DetectedEpochs           int64
	UncertainEventLowerBound int64
}

const staleEpochsSQL = `SELECT gateway_instance, process_epoch::text, accepted, persisted, abandoned,
        updated_at, stale_candidate_at
    FROM olp_go.request_metadata_gateway_epochs
    WHERE gracefully_closed_at IS NULL AND stale_detected_at IS NULL AND updated_at < $1
    ORDER BY updated_at, gateway_instance, process_epoch
    LIMIT 100 FOR UPDATE SKIP LOCKED`

const markCandidateSQL = `UPDATE olp_go.request_metadata_gateway_epochs SET stale_candidate_at = $1
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
				if err := CheckpointTask(ctx, pool, TaskEpochDetection, OutcomeFailure, false); err != nil {
					log.Warn("stale gateway epoch checkpoint failed", "error", err)
				}
				continue
			}
			progress := report.CandidateEpochs > 0 || report.DetectedEpochs > 0
			if err := CheckpointTask(ctx, pool, TaskEpochDetection, OutcomeSuccess, progress); err != nil {
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
