//go:build integration

package usage

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIntegrationBudgetAccountingLossSurvivesReplicaRestart(t *testing.T) {
	pool := notificationPool(t)
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
}
