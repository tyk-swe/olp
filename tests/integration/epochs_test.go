//go:build integration

package integration_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/usage"
)

// acctEpochRow is the durable delivery accounting of one gateway process.
type acctEpochRow struct {
	Accepted           int64
	Persisted          int64
	Dropped            int64
	Abandoned          int64
	Retrying           bool
	WriterClosed       bool
	GracefullyClosedAt *time.Time
	StaleCandidateAt   *time.Time
	StaleDetectedAt    *time.Time
	UncertaintyGapID   *string
}

func acctLoadEpoch(t *testing.T, pool *pgxpool.Pool, instance, epoch string) acctEpochRow {
	t.Helper()
	var row acctEpochRow
	err := pool.QueryRow(t.Context(), `SELECT accepted, persisted, dropped, abandoned, retrying,
            writer_closed, gracefully_closed_at, stale_candidate_at, stale_detected_at,
            uncertainty_gap_id::text
        FROM olp.request_metadata_gateway_epochs
        WHERE gateway_instance = $1 AND process_epoch = $2::uuid`, instance, epoch).
		Scan(&row.Accepted, &row.Persisted, &row.Dropped, &row.Abandoned, &row.Retrying,
			&row.WriterClosed, &row.GracefullyClosedAt, &row.StaleCandidateAt,
			&row.StaleDetectedAt, &row.UncertaintyGapID)
	if err != nil {
		t.Fatalf("load epoch %s/%s: %v", instance, epoch, err)
	}
	return row
}

// acctGapRow is one admission that request metadata never became usage.
type acctGapRow struct {
	EventCount int64
	Reason     string
	Certainty  string
	First      time.Time
	Last       time.Time
}

func acctGaps(t *testing.T, pool *pgxpool.Pool, instance string) []acctGapRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT event_count, reason, certainty,
            first_observed_at, last_observed_at
        FROM olp.request_metadata_ingestion_gaps WHERE gateway_instance = $1
        ORDER BY reported_at, id`, instance)
	if err != nil {
		t.Fatalf("query gaps: %v", err)
	}
	defer rows.Close()
	var gaps []acctGapRow
	for rows.Next() {
		var gap acctGapRow
		if err := rows.Scan(&gap.EventCount, &gap.Reason, &gap.Certainty,
			&gap.First, &gap.Last); err != nil {
			t.Fatalf("scan gap: %v", err)
		}
		gaps = append(gaps, gap)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read gaps: %v", err)
	}
	return gaps
}

// acctSameInstant compares a timestamp with one that made a round trip through
// PostgreSQL, which keeps microseconds rather than nanoseconds.
func acctSameInstant(t *testing.T, got, want time.Time, what string) {
	t.Helper()
	if difference := got.Sub(want); difference > time.Microsecond || difference < -time.Microsecond {
		t.Fatalf("%s = %s, want %s", what, got, want)
	}
}

func acctInstance(t *testing.T) string {
	t.Helper()
	return "gateway-" + acctID(t)
}

// Only what was lost since the previous checkpoint is reported again.

// The window opens no earlier than the last checkpoint: everything before
// it was already accounted for.

// Counters can only ever move forwards.

// A retried close of the same state is the same promise, not a new one.

// A closed epoch cannot come back to life with different counters.

// An undrained writer must not claim a clean shutdown.

// The detector leaves a cleanly closed epoch alone forever.

// Four more events were accepted and never accounted for either way.

// Three detectors race exactly as three replicas would.

// A candidate that stays silent past the confirmation delay is resolved once.

// Nothing is left for a later pass to find.

// acctDetectConcurrently runs several detectors at once and sums what they
// each claimed, which must add up to one pass over the epoch.
func acctDetectConcurrently(t *testing.T, pool *pgxpool.Pool, now time.Time,
	replicas int) usage.Detection {
	t.Helper()
	results := make([]usage.Detection, replicas)
	failures := make([]error, replicas)
	var wait sync.WaitGroup
	wait.Add(replicas)
	for index := range replicas {
		go func() {
			defer wait.Done()
			results[index], failures[index] = usage.DetectStaleEpochs(t.Context(), pool, now)
		}()
	}
	wait.Wait()
	var total usage.Detection
	for index, err := range failures {
		if err != nil {
			t.Fatalf("detector %d: %v", index, err)
		}
		total.CandidateEpochs += results[index].CandidateEpochs
		total.DetectedEpochs += results[index].DetectedEpochs
		total.UncertainEventLowerBound += results[index].UncertainEventLowerBound
	}
	return total
}

func TestConsumerHealthKeepsTheFreshestSample(t *testing.T) {
	t.Parallel()
	pool := acctPool(t)
	now := time.Now().UTC()
	oldest := now.Add(-30 * time.Second)
	lagged := now.Add(-20 * time.Second)
	if err := usage.ReportConsumerHealth(t.Context(), pool, 3, 5, &oldest, &lagged, now); err != nil {
		t.Fatalf("report health: %v", err)
	}
	pending, lag, recorded := acctConsumerHealth(t, pool)
	if pending != 3 || lag != 5 || recorded == nil {
		t.Fatalf("health = %d/%d/%v, want the reported backlog", pending, lag, recorded)
	}
	acctSameInstant(t, *recorded, oldest, "oldest pending delivery")

	// An older reading from a second consumer must not undo the newer one.
	if err := usage.ReportConsumerHealth(t.Context(), pool, 0, 0, nil, nil,
		now.Add(-time.Minute)); err != nil {
		t.Fatalf("report stale health: %v", err)
	}
	if pending, lag, _ = acctConsumerHealth(t, pool); pending != 3 || lag != 5 {
		t.Fatalf("health = %d/%d, want the newer sample to survive", pending, lag)
	}

	// A later reading replaces it.
	if err := usage.ReportConsumerHealth(t.Context(), pool, 0, 0, nil, nil,
		time.Now().UTC().Add(time.Second)); err != nil {
		t.Fatalf("report drained health: %v", err)
	}
	pending, lag, recorded = acctConsumerHealth(t, pool)
	if pending != 0 || lag != 0 || recorded != nil {
		t.Fatalf("health = %d/%d/%v, want a drained group", pending, lag, recorded)
	}

	// Reporting health is progress the worker supervisor can see.
	var successes int64
	if err := pool.QueryRow(t.Context(), `SELECT successes_total FROM olp.worker_task_health
        WHERE task = 'request_metadata_consumer'`).Scan(&successes); err != nil {
		t.Fatalf("load task health: %v", err)
	}
	if successes < 3 {
		t.Fatalf("successes = %d, want one per accepted sample", successes)
	}

	future := time.Now().UTC().Add(time.Hour)
	contradictions := []struct {
		name    string
		pending int64
		lag     int64
		oldest  *time.Time
		lagged  *time.Time
	}{
		{name: "a backlog with nothing pending", pending: 4},
		{name: "an empty group with something pending", oldest: &oldest},
		{name: "a lag with nothing undelivered", lag: 2},
		{name: "an undelivered entry with no lag", lagged: &oldest},
		{name: "a negative backlog", pending: -1},
		{name: "a negative lag", lag: -1},
		{name: "a delivery from the future", pending: 1, oldest: &future},
	}
	for _, test := range contradictions {
		t.Run(test.name, func(t *testing.T) {
			err := usage.ReportConsumerHealth(t.Context(), pool, test.pending, test.lag,
				test.oldest, test.lagged, time.Now().UTC())
			if !errors.Is(err, usage.ErrInvalidCheckpoint) {
				t.Fatalf("error = %v, want ErrInvalidCheckpoint", err)
			}
		})
	}
}

func acctConsumerHealth(t *testing.T, pool *pgxpool.Pool) (int64, int64, *time.Time) {
	t.Helper()
	var pending, lag int64
	var oldest *time.Time
	err := pool.QueryRow(t.Context(), `SELECT pending_events, lag_events, oldest_pending_at
        FROM olp.request_metadata_consumer_health WHERE singleton`).Scan(&pending, &lag, &oldest)
	if err != nil {
		t.Fatalf("load consumer health: %v", err)
	}
	return pending, lag, oldest
}
