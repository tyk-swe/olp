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
