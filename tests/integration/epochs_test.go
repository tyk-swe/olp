//go:build integration

package integration_test

import (
	"errors"
	"strings"
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

func TestGatewayEpochCheckpointsBufferLossExactly(t *testing.T) {
	t.Parallel()
	pool := acctPool(t)
	instance, epoch := acctInstance(t), acctID(t)
	started := time.Now().UTC().Add(-time.Minute)
	firstLoss := started.Add(10 * time.Second)
	snapshot := usage.Snapshot{ProcessEpoch: epoch, StartedAt: started, Accepted: 10,
		Persisted: 8, Dropped: 2, FirstLossAt: &firstLoss, LastLossAt: &firstLoss}

	report, err := usage.CheckpointEpoch(t.Context(), pool, instance, snapshot, false)
	if err != nil {
		t.Fatalf("first checkpoint: %v", err)
	}
	if report.ReportedEvents != 2 || report.ReportedDropped != 2 || report.ReportedAbandoned != 0 {
		t.Fatalf("report = %+v, want two dropped events", report)
	}
	if !report.ProcessEpochChanged {
		t.Fatal("the first checkpoint of an epoch did not report the change")
	}
	gaps := acctGaps(t, pool, instance)
	if len(gaps) != 1 {
		t.Fatalf("gaps = %+v, want one", gaps)
	}
	if gaps[0].Reason != "gateway_local_buffer_loss" || gaps[0].EventCount != 2 ||
		gaps[0].Certainty != "exact" {
		t.Fatalf("gap = %+v, want two exactly counted lost events", gaps[0])
	}
	acctSameInstant(t, gaps[0].First, firstLoss, "gap window start")
	acctSameInstant(t, gaps[0].Last, firstLoss, "gap window end")
	row := acctLoadEpoch(t, pool, instance, epoch)
	if row.Accepted != 10 || row.Persisted != 8 || row.Dropped != 2 {
		t.Fatalf("epoch = %+v, want the reported counters", row)
	}
	if row.GracefullyClosedAt != nil || row.StaleDetectedAt != nil {
		t.Fatalf("epoch = %+v, want it left open", row)
	}

	// Only what was lost since the previous checkpoint is reported again.
	lastLoss := time.Now().UTC()
	progressed := usage.Snapshot{ProcessEpoch: epoch, StartedAt: started, Accepted: 20,
		Persisted: 15, Dropped: 3, Abandoned: 2, Retrying: true,
		FirstLossAt: &firstLoss, LastLossAt: &lastLoss}
	report, err = usage.CheckpointEpoch(t.Context(), pool, instance, progressed, false)
	if err != nil {
		t.Fatalf("second checkpoint: %v", err)
	}
	if report.ReportedEvents != 3 || report.ReportedDropped != 1 || report.ReportedAbandoned != 2 {
		t.Fatalf("report = %+v, want only the new loss", report)
	}
	if report.ProcessEpochChanged {
		t.Fatal("a continuing epoch reported a change of process")
	}
	gaps = acctGaps(t, pool, instance)
	if len(gaps) != 2 || gaps[1].EventCount != 3 {
		t.Fatalf("gaps = %+v, want a second gap of three events", gaps)
	}
	// The window opens no earlier than the last checkpoint: everything before
	// it was already accounted for.
	if gaps[1].First.Before(gaps[0].Last) {
		t.Fatalf("gap window %s reaches behind the previous checkpoint", gaps[1].First)
	}
	if row = acctLoadEpoch(t, pool, instance, epoch); !row.Retrying || row.Abandoned != 2 {
		t.Fatalf("epoch = %+v, want the retrying writer recorded", row)
	}

	// Counters can only ever move forwards.
	regressed := progressed
	regressed.Persisted = 14
	if _, err := usage.CheckpointEpoch(t.Context(), pool, instance, regressed, false); err == nil {
		t.Fatal("a regressed counter was accepted")
	}
	if after := acctLoadEpoch(t, pool, instance, epoch); after.Persisted != 15 {
		t.Fatalf("epoch = %+v, want the rejected checkpoint to change nothing", after)
	}
}

func TestGatewayEpochGracefulCloseIsIdempotentAndFinal(t *testing.T) {
	t.Parallel()
	pool := acctPool(t)
	instance, epoch := acctInstance(t), acctID(t)
	started := time.Now().UTC().Add(-2 * time.Minute)
	drained := usage.Snapshot{ProcessEpoch: epoch, StartedAt: started, Accepted: 5,
		Persisted: 5, Closed: true}

	if _, err := usage.CheckpointEpoch(t.Context(), pool, instance, drained, true); err != nil {
		t.Fatalf("close epoch: %v", err)
	}
	row := acctLoadEpoch(t, pool, instance, epoch)
	if row.GracefullyClosedAt == nil {
		t.Fatal("a drained epoch was not closed")
	}
	closedAt := *row.GracefullyClosedAt

	// A retried close of the same state is the same promise, not a new one.
	report, err := usage.CheckpointEpoch(t.Context(), pool, instance, drained, true)
	if err != nil {
		t.Fatalf("repeat close: %v", err)
	}
	if report.ReportedEvents != 0 || report.ProcessEpochChanged {
		t.Fatalf("report = %+v, want a silent no-op", report)
	}
	if row = acctLoadEpoch(t, pool, instance, epoch); row.GracefullyClosedAt == nil ||
		!row.GracefullyClosedAt.Equal(closedAt) {
		t.Fatalf("epoch = %+v, want the original close time %s", row, closedAt)
	}

	// A closed epoch cannot come back to life with different counters.
	moved := drained
	moved.Accepted, moved.Persisted = 6, 6
	if _, err := usage.CheckpointEpoch(t.Context(), pool, instance, moved, true); err == nil {
		t.Fatal("a closed epoch accepted new counters")
	}
	if _, err := usage.CheckpointEpoch(t.Context(), pool, instance, drained, false); err == nil {
		t.Fatal("a closed epoch accepted a further checkpoint")
	}

	// An undrained writer must not claim a clean shutdown.
	open := usage.Snapshot{ProcessEpoch: acctID(t), StartedAt: started, Accepted: 3, Persisted: 1}
	if _, err := usage.CheckpointEpoch(t.Context(), pool, instance, open, true); err == nil {
		t.Fatal("an epoch with outstanding events closed gracefully")
	}

	// The detector leaves a cleanly closed epoch alone forever.
	detection, err := usage.DetectStaleEpochs(t.Context(), pool, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("detect stale epochs: %v", err)
	}
	if detection.CandidateEpochs != 0 || detection.DetectedEpochs != 0 {
		t.Fatalf("detection = %+v, want a closed epoch to be ignored", detection)
	}
	if gaps := acctGaps(t, pool, instance); len(gaps) != 0 {
		t.Fatalf("gaps = %+v, want a clean shutdown to leave no scar", gaps)
	}
}

func TestGatewayEpochSupersededByANewProcessIsRecordedAsUnclean(t *testing.T) {
	t.Parallel()
	pool := acctPool(t)
	instance := acctInstance(t)
	crashed, replacement := acctID(t), acctID(t)
	started := time.Now().UTC().Add(-3 * time.Minute)
	lost := usage.Snapshot{ProcessEpoch: crashed, StartedAt: started, Accepted: 9,
		Persisted: 4, Abandoned: 1}
	if _, err := usage.CheckpointEpoch(t.Context(), pool, instance, lost, false); err != nil {
		t.Fatalf("checkpoint crashed process: %v", err)
	}

	fresh := usage.Snapshot{ProcessEpoch: replacement, StartedAt: time.Now().UTC().Add(-time.Second)}
	report, err := usage.CheckpointEpoch(t.Context(), pool, instance, fresh, false)
	if err != nil {
		t.Fatalf("checkpoint replacement process: %v", err)
	}
	if !report.ProcessEpochChanged {
		t.Fatal("a new process did not report that the epoch changed")
	}

	previous := acctLoadEpoch(t, pool, instance, crashed)
	if previous.StaleDetectedAt == nil || previous.UncertaintyGapID == nil {
		t.Fatalf("crashed epoch = %+v, want it resolved as unclean", previous)
	}
	if current := acctLoadEpoch(t, pool, instance, replacement); current.StaleDetectedAt != nil ||
		current.GracefullyClosedAt != nil {
		t.Fatalf("replacement epoch = %+v, want it left open", current)
	}
	gaps := acctGaps(t, pool, instance)
	if len(gaps) != 2 {
		t.Fatalf("gaps = %+v, want the abandoned event and the unclean shutdown", gaps)
	}
	if gaps[0].Reason != "gateway_local_buffer_loss" || gaps[0].EventCount != 1 {
		t.Fatalf("gap = %+v, want the one event the writer abandoned", gaps[0])
	}
	// Four more events were accepted and never accounted for either way.
	if gaps[1].Reason != "gateway_epoch_unclean_shutdown" || gaps[1].EventCount != 4 ||
		gaps[1].Certainty != "lower_bound" {
		t.Fatalf("gap = %+v, want a lower bound of four lost events", gaps[1])
	}
}

func TestGatewayEpochDetectionConfirmsEachStaleEpochOnce(t *testing.T) {
	t.Parallel()
	pool := acctPool(t)
	instance, epoch := acctInstance(t), acctID(t)
	silent := time.Now().UTC().Add(-5 * time.Minute)
	acctExec(t, pool, `INSERT INTO olp.request_metadata_gateway_epochs
            (gateway_instance, process_epoch, started_at, accepted, persisted, dropped,
             abandoned, retrying, writer_closed, updated_at)
        VALUES ($1, $2::uuid, $3, 7, 2, 0, 1, false, false, $3)`, instance, epoch, silent)

	// Three detectors race exactly as three replicas would.
	now := time.Now().UTC()
	first := acctDetectConcurrently(t, pool, now, 3)
	if first.CandidateEpochs != 1 || first.DetectedEpochs != 0 {
		t.Fatalf("first pass = %+v, want a single unconfirmed candidate", first)
	}
	if row := acctLoadEpoch(t, pool, instance, epoch); row.StaleCandidateAt == nil ||
		row.StaleDetectedAt != nil {
		t.Fatalf("epoch = %+v, want a candidate that is not yet resolved", row)
	}
	if gaps := acctGaps(t, pool, instance); len(gaps) != 0 {
		t.Fatalf("gaps = %+v, want none before the candidate is confirmed", gaps)
	}

	// A candidate that stays silent past the confirmation delay is resolved once.
	second := acctDetectConcurrently(t, pool, now.Add(11*time.Second), 3)
	if second.DetectedEpochs != 1 || second.UncertainEventLowerBound != 4 {
		t.Fatalf("second pass = %+v, want one epoch resolved with four uncertain events", second)
	}
	row := acctLoadEpoch(t, pool, instance, epoch)
	if row.StaleDetectedAt == nil || row.UncertaintyGapID == nil {
		t.Fatalf("epoch = %+v, want it resolved with a gap", row)
	}
	gaps := acctGaps(t, pool, instance)
	if len(gaps) != 1 || gaps[0].Reason != "gateway_epoch_unclean_shutdown" ||
		gaps[0].EventCount != 4 {
		t.Fatalf("gaps = %+v, want exactly one unclean shutdown gap of four events", gaps)
	}

	// Nothing is left for a later pass to find.
	third := acctDetectConcurrently(t, pool, now.Add(time.Minute), 3)
	if third.CandidateEpochs != 0 || third.DetectedEpochs != 0 {
		t.Fatalf("third pass = %+v, want a resolved epoch to be ignored", third)
	}
}

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

func TestRequestMetadataGapsAreReportedOnlyOnce(t *testing.T) {
	t.Parallel()
	pool := acctPool(t)
	instance := acctInstance(t)
	observed := time.Now().UTC()
	gap := usage.Gap{GatewayInstance: instance, EventCount: 1,
		Reason: "missing_stream_event", FirstObservedAt: observed, LastObservedAt: observed}

	inserted, err := usage.ReportGapOnce(t.Context(), pool, gap, "request-metadata-stream:7-0:missing")
	if err != nil || !inserted {
		t.Fatalf("first report = %v, %v; want it recorded", inserted, err)
	}
	inserted, err = usage.ReportGapOnce(t.Context(), pool, gap, "request-metadata-stream:7-0:missing")
	if err != nil || inserted {
		t.Fatalf("repeat report = %v, %v; want it ignored", inserted, err)
	}
	if inserted, err = usage.ReportGapOnce(t.Context(), pool, gap,
		"request-metadata-stream:8-0:missing"); err != nil || !inserted {
		t.Fatalf("second delivery = %v, %v; want it recorded", inserted, err)
	}
	if gaps := acctGaps(t, pool, instance); len(gaps) != 2 {
		t.Fatalf("gaps = %+v, want two", gaps)
	}

	cases := []struct {
		name  string
		gap   usage.Gap
		key   string
		valid bool
	}{
		{name: "a missing key cannot deduplicate", gap: gap, key: ""},
		{name: "a blank key cannot deduplicate", gap: gap, key: "  "},
		{name: "an oversized key would not fit the row", gap: gap,
			key: strings.Repeat("k", 257)},
		{name: "an empty gap admits nothing", key: "empty",
			gap: usage.Gap{GatewayInstance: instance, Reason: "missing_stream_event",
				FirstObservedAt: observed, LastObservedAt: observed}},
		{name: "a gap needs a gateway", key: "no-instance",
			gap: usage.Gap{EventCount: 1, Reason: "missing_stream_event",
				FirstObservedAt: observed, LastObservedAt: observed}},
		{name: "a gap needs a reason", key: "no-reason",
			gap: usage.Gap{GatewayInstance: instance, EventCount: 1,
				FirstObservedAt: observed, LastObservedAt: observed}},
		{name: "a gap cannot end before it starts", key: "reversed",
			gap: usage.Gap{GatewayInstance: instance, EventCount: 1,
				Reason: "missing_stream_event", FirstObservedAt: observed,
				LastObservedAt: observed.Add(-time.Second)}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := usage.ReportGapOnce(t.Context(), pool, test.gap, test.key); err == nil {
				t.Fatal("an unusable gap was accepted")
			}
		})
	}
}

func TestConsumerHealthKeepsTheFreshestSample(t *testing.T) {
	t.Parallel()
	pool := acctPool(t)
	now := time.Now().UTC()
	oldest := now.Add(-30 * time.Second)
	if err := usage.ReportConsumerHealth(t.Context(), pool, 3, 5, &oldest, now); err != nil {
		t.Fatalf("report health: %v", err)
	}
	pending, lag, recorded := acctConsumerHealth(t, pool)
	if pending != 3 || lag != 5 || recorded == nil {
		t.Fatalf("health = %d/%d/%v, want the reported backlog", pending, lag, recorded)
	}
	acctSameInstant(t, *recorded, oldest, "oldest pending delivery")

	// An older reading from a second consumer must not undo the newer one.
	if err := usage.ReportConsumerHealth(t.Context(), pool, 0, 0, nil,
		now.Add(-time.Minute)); err != nil {
		t.Fatalf("report stale health: %v", err)
	}
	if pending, lag, _ = acctConsumerHealth(t, pool); pending != 3 || lag != 5 {
		t.Fatalf("health = %d/%d, want the newer sample to survive", pending, lag)
	}

	// A later reading replaces it.
	if err := usage.ReportConsumerHealth(t.Context(), pool, 0, 0, nil,
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
	}{
		{name: "a backlog with nothing pending", pending: 4},
		{name: "an empty group with something pending", oldest: &oldest},
		{name: "a negative backlog", pending: -1},
		{name: "a negative lag", lag: -1},
		{name: "a delivery from the future", pending: 1, oldest: &future},
	}
	for _, test := range contradictions {
		t.Run(test.name, func(t *testing.T) {
			err := usage.ReportConsumerHealth(t.Context(), pool, test.pending, test.lag,
				test.oldest, time.Now().UTC())
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
