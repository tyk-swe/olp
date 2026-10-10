//go:build integration

package usage

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestIntegrationBudgetAccountingLossSurvivesReplicaRestart(t *testing.T) {
	pool := notificationPool(t)
	liveConsumerHeartbeat(t, pool)
	local := NewEmitter(1)
	if err := CheckBudgetAccounting(t.Context(), pool, local); err != nil {
		t.Fatal(err)
	}
	local.Drop()
	if err := CheckBudgetAccounting(t.Context(), pool, local); !errors.Is(err, ErrIncompleteBudgetAccounting) {
		t.Fatalf("uncheckpointed local loss admitted: %v", err)
	}
	if err := RecordBudgetLoss(t.Context(), pool, local, "budget-test", nil); err != nil {
		t.Fatal(err)
	}
	if err := RecordBudgetLoss(t.Context(), pool, local, "budget-test", nil); err != nil {
		t.Fatal(err)
	}
	restarted := NewEmitter(1)
	if err := CheckBudgetAccounting(t.Context(), pool, restarted); !errors.Is(err, ErrIncompleteBudgetAccounting) {
		t.Fatalf("replica admitted despite durable gap: %v", err)
	}
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.request_metadata_ingestion_gaps WHERE gateway_instance='budget-test'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("duplicate loss: %d %v", n, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.request_metadata_ingestion_gaps SET first_observed_at=now()-interval '2 months', last_observed_at=now()-interval '2 months'`); err != nil {
		t.Fatal(err)
	}
	local.lastLossMS.Store(time.Now().AddDate(0, -2, 0).UnixMilli())
	if err := CheckBudgetAccounting(t.Context(), pool, local); err != nil {
		t.Fatalf("old local loss blocked fresh budget month: %v", err)
	}
	if err := CheckBudgetAccounting(t.Context(), pool, restarted); err != nil {
		t.Fatalf("old gap blocked fresh budget month: %v", err)
	}
}

func TestIntegrationBudgetAccountingUsesEveryActiveCalendarWindow(t *testing.T) {
	for _, tc := range []struct{ name, setup, now, lost string }{
		{"week before month", "", "2026-08-01T12:00:00Z", "2026-07-30T12:00:00Z"},
		{"non UTC month", "INSERT INTO olp.budget_calendar_changes SELECT k,'2025-01-01'::timestamptz,'Asia/Kathmandu' FROM unnest(ARRAY['day','week','month']) k", "2026-01-01T00:00:00Z", "2025-12-31T20:00:00Z"},
		{"transition day before month and week", "SELECT olp.schedule_budget_zone('Asia/Kathmandu','2026-05-30T12:00:00Z'::timestamptz)", "2026-06-01T00:00:00Z", "2026-05-31T19:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := notificationPool(t)
			liveConsumerHeartbeat(t, pool)
			if tc.setup != "" {
				if _, err := pool.Exec(t.Context(), tc.setup); err != nil {
					t.Fatal(err)
				}
			}
			now, _ := time.Parse(time.RFC3339, tc.now)
			lost, _ := time.Parse(time.RFC3339, tc.lost)
			query := strings.ReplaceAll(budgetAccountingSQL, "now()", "$3::timestamptz")
			var incomplete bool
			if err := pool.QueryRow(t.Context(), query, EpochStaleAfter.Seconds(), lost, now).Scan(&incomplete); err != nil || !incomplete {
				t.Fatalf("local loss in active calendar admitted: %v %v", incomplete, err)
			}
			if _, err := ReportGapOnce(t.Context(), pool, Gap{GatewayInstance: "calendar-test", EventCount: 1, Reason: "lost", FirstObservedAt: lost, LastObservedAt: lost}, "calendar-test"); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(t.Context(), query, EpochStaleAfter.Seconds(), nil, now).Scan(&incomplete); err != nil || !incomplete {
				t.Fatalf("durable loss in active calendar admitted: %v %v", incomplete, err)
			}
		})
	}
}

func TestIntegrationBudgetAccountingRetainsRolledUpLoss(t *testing.T) {
	pool := notificationPool(t)
	liveConsumerHeartbeat(t, pool)
	local := NewEmitter(1)
	local.Drop()
	if err := RecordBudgetLoss(t.Context(), pool, local, "rolled-budget-test", nil); err != nil {
		t.Fatal(err)
	}
	var rolled, removed int64
	if err := pool.QueryRow(t.Context(), gapRollupSQL, time.Now().Add(time.Hour), int64(ReplayHorizonDays), int64(FutureSkewMinutes)).Scan(&rolled, &removed); err != nil {
		t.Fatal(err)
	}
	if rolled != 1 || removed != 1 {
		t.Fatalf("gap did not roll into hourly evidence: %d/%d", rolled, removed)
	}
	if err := CheckBudgetAccounting(t.Context(), pool, NewEmitter(1)); !errors.Is(err, ErrIncompleteBudgetAccounting) {
		t.Fatalf("rolled-up loss reopened budget: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.request_metadata_gap_hourly SET bucket=date_trunc('hour', (now()-interval '2 months') AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', first_observed_at=now()-interval '2 months', last_observed_at=now()-interval '2 months'`); err != nil {
		t.Fatal(err)
	}
	if err := CheckBudgetAccounting(t.Context(), pool, NewEmitter(1)); err != nil {
		t.Fatalf("old rollup blocked fresh month: %v", err)
	}
}

func TestIntegrationBudgetAccountingRefusesStaleEpochWithoutDetector(t *testing.T) {
	pool := notificationPool(t)
	liveConsumerHeartbeat(t, pool)
	crashed := NewEmitter(1)
	crashed.startedAt = time.Now().Add(-3 * EpochStaleAfter)
	// Production registers this zero-counter epoch before binding listeners.
	// Simulate a crash whose immediate loss write and next heartbeat never landed.
	if _, err := CheckpointEpoch(t.Context(), pool, "crashed-before-checkpoint", crashed.Snapshot(), false); err != nil {
		t.Fatal(err)
	}
	if err := CheckBudgetAccounting(t.Context(), pool, NewEmitter(1)); err != nil {
		t.Fatalf("fresh live epoch rejected: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.request_metadata_gateway_epochs SET updated_at=now()-make_interval(secs => $1) WHERE gateway_instance='crashed-before-checkpoint'`, 2*EpochStaleAfter.Seconds()); err != nil {
		t.Fatal(err)
	}
	// Do not call DetectStaleEpochs: admission must protect budgets even without
	// a running recovery worker and even though no durable event counter advanced.
	if err := CheckBudgetAccounting(t.Context(), pool, NewEmitter(1)); !errors.Is(err, ErrIncompleteBudgetAccounting) {
		t.Fatalf("stale unclosed epoch admitted: %v", err)
	}
	if _, err := CheckpointEpoch(t.Context(), pool, "crashed-before-checkpoint", crashed.Snapshot(), false); err != nil {
		t.Fatal(err)
	}
	if err := CheckBudgetAccounting(t.Context(), pool, NewEmitter(1)); err != nil {
		t.Fatalf("resumed healthy heartbeat did not recover: %v", err)
	}
	// An epoch whose stream writes are still retrying keeps checkpointing
	// while terminal events sit in the producer's buffer: the reservation
	// grace cannot outlast that backlog, so admission stays closed.
	if _, err := pool.Exec(t.Context(), `UPDATE olp.request_metadata_gateway_epochs SET retrying=true WHERE gateway_instance='crashed-before-checkpoint'`); err != nil {
		t.Fatal(err)
	}
	if err := CheckBudgetAccounting(t.Context(), pool, NewEmitter(1)); !errors.Is(err, ErrIncompleteBudgetAccounting) {
		t.Fatalf("retrying epoch admitted: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.request_metadata_gateway_epochs SET retrying=false WHERE gateway_instance='crashed-before-checkpoint'`); err != nil {
		t.Fatal(err)
	}
	if err := CheckBudgetAccounting(t.Context(), pool, NewEmitter(1)); err != nil {
		t.Fatalf("delivered epoch blocked admission: %v", err)
	}
}

func TestIntegrationBudgetAccountingRefusesUnconsumedBacklog(t *testing.T) {
	pool := notificationPool(t)
	old := time.Now().Add(-2 * EpochStaleAfter)
	// A live consumer reporting entries pending past the stale window means
	// durable usage is not reaching accounting; a version this build cannot
	// decode stays pending until a compatible consumer processes it.
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.request_metadata_consumer_health
		(singleton, pending_events, lag_events, oldest_pending_at, checked_at)
		VALUES(true,1,0,$1,now())`, old); err != nil {
		t.Fatal(err)
	}
	if err := CheckBudgetAccounting(t.Context(), pool, NewEmitter(1)); !errors.Is(err, ErrIncompleteBudgetAccounting) {
		t.Fatalf("unconsumed backlog admitted: %v", err)
	}
	// A fresh pending entry is ordinary in-flight work, not lost usage.
	if _, err := pool.Exec(t.Context(), `UPDATE olp.request_metadata_consumer_health SET oldest_pending_at=now()`); err != nil {
		t.Fatal(err)
	}
	if err := CheckBudgetAccounting(t.Context(), pool, NewEmitter(1)); err != nil {
		t.Fatalf("fresh backlog blocked admission: %v", err)
	}
	// Stranded undelivered entries trail durable accounting the same way:
	// reported usage stays behind the stream while producers keep writing.
	if _, err := pool.Exec(t.Context(), `UPDATE olp.request_metadata_consumer_health
		SET lag_events=2, oldest_lagged_at=$1`, old); err != nil {
		t.Fatal(err)
	}
	if err := CheckBudgetAccounting(t.Context(), pool, NewEmitter(1)); !errors.Is(err, ErrIncompleteBudgetAccounting) {
		t.Fatalf("stranded lag admitted: %v", err)
	}
	// Fresh undelivered entries are ordinary in-flight work.
	if _, err := pool.Exec(t.Context(), `UPDATE olp.request_metadata_consumer_health SET oldest_lagged_at=now()`); err != nil {
		t.Fatal(err)
	}
	if err := CheckBudgetAccounting(t.Context(), pool, NewEmitter(1)); err != nil {
		t.Fatalf("fresh lag blocked admission: %v", err)
	}
	// A stale heartbeat cannot speak for the group: a dead consumer leaves
	// deliveries pending while producers keep writing, so admission stays
	// closed until liveness is proven again.
	if _, err := pool.Exec(t.Context(), `UPDATE olp.request_metadata_consumer_health SET oldest_pending_at=$1, checked_at=$1`, old); err != nil {
		t.Fatal(err)
	}
	if err := CheckBudgetAccounting(t.Context(), pool, NewEmitter(1)); !errors.Is(err, ErrIncompleteBudgetAccounting) {
		t.Fatalf("silent consumer admitted: %v", err)
	}
	// A missing heartbeat cannot either: the consumer may never have run.
	if _, err := pool.Exec(t.Context(), `DELETE FROM olp.request_metadata_consumer_health`); err != nil {
		t.Fatal(err)
	}
	if err := CheckBudgetAccounting(t.Context(), pool, NewEmitter(1)); !errors.Is(err, ErrIncompleteBudgetAccounting) {
		t.Fatalf("absent consumer admitted: %v", err)
	}
	// A fresh healthy heartbeat reopens admission.
	liveConsumerHeartbeat(t, pool)
	if err := CheckBudgetAccounting(t.Context(), pool, NewEmitter(1)); err != nil {
		t.Fatalf("healthy consumer blocked admission: %v", err)
	}
}

// liveConsumerHeartbeat seeds the single consumer health row the way a live
// consumer's five-second checkpoint does, since a missing or stale heartbeat
// closes cost-budget admission.
func liveConsumerHeartbeat(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.request_metadata_consumer_health
		(singleton, pending_events, lag_events, oldest_pending_at, checked_at) VALUES(true,0,0,NULL,now())
		ON CONFLICT (singleton) DO UPDATE SET pending_events=0, lag_events=0,
			oldest_pending_at=NULL, oldest_lagged_at=NULL, checked_at=now()`); err != nil {
		t.Fatal(err)
	}
}
