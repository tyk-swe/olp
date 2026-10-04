//go:build integration

package usage

import (
	"errors"
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
