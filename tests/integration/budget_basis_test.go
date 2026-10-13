//go:build integration

package integration_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/limits"
)

// TestBudgetBasisInstallsIndependentCounters proves a charge-basis snapshot
// lands on the owner's "{owner}:charge:*" keys without touching its cost
// counters, and that a charge-basis reservation is judged from the charge
// balance alone.
func TestBudgetBasisInstallsIndependentCounters(t *testing.T) {
	f := limCostSeed(t, "basis_counters", "0")
	windows := f.windows
	// Cost accrued on the cost counters only.
	f.accrue(t, "", "0.8")
	if _, _, err := f.limiter.ApplyCostSnapshot(t.Context(), limits.CostSnapshot{
		Basis: limits.BasisCharge, CostOwnerID: f.owner,
		DailyWindowID: windows.DailyID, DailyAccrued: "0.9",
		MonthlyWindowID: windows.MonthlyID, MonthlyAccrued: "0.9",
	}); err != nil {
		t.Fatalf("ApplyCostSnapshot charge: %v", err)
	}
	day, _ := limCostKeys(f.namespace, f.owner)
	if got := limHash(t, f.c, day)["accrued"]; got != "0.8" {
		t.Fatalf("cost day = %q, want 0.8 untouched by the charge snapshot", got)
	}
	chargeDay := strings.Replace(day, ":cost:day", ":charge:day", 1)
	if got := limHash(t, f.c, chargeDay)["accrued"]; got != "0.9" {
		t.Fatalf("charge day = %q, want 0.9", got)
	}

	// A cost-basis request sees 0.8 accrued; the same request on the charge
	// basis sees 0.9, so a request that fits one basis refuses the other.
	request := limits.Request{
		CostOwnerID: f.owner, LookupID: limLookup(),
		DailyCostLimit: limPointer("1"),
		LeaseTTL:       5 * time.Second, CostEstimate: "0.15", RequestID: uuid.NewString(),
	}
	if lease, err := f.limiter.Reserve(t.Context(), request); err != nil {
		t.Fatalf("a cost-basis request beside 0.8 of 1 must fit: %v", err)
	} else if err = lease.Refund(t.Context()); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	request.CostBasis = limits.BasisCharge
	request.RequestID = uuid.NewString()
	if _, err := f.limiter.Reserve(t.Context(), request); err == nil {
		t.Fatal("a charge-basis request beside 0.9 of 1 must refuse")
	}

	// Switching the same owner's basis back to cost sees the cost balance
	// immediately; no state migrates.
	request.CostBasis = limits.BasisCost
	request.RequestID = uuid.NewString()
	if _, err := f.limiter.Reserve(t.Context(), request); err != nil {
		t.Fatalf("a cost-basis reservation after a basis switch must fit: %v", err)
	}
}

// TestChargeReservationRemovesByRequestOnItsBasis proves a charge snapshot
// naming a request releases that request's charge pending reservation and not
// a cost reservation under the same identity.
func TestChargeReservationRemovesByRequestOnItsBasis(t *testing.T) {
	f := limCostSeed(t, "basis_pending", "0")
	// Initialize the charge counters as accounting would.
	if _, _, err := f.limiter.ApplyCostSnapshot(t.Context(), limits.CostSnapshot{
		Basis: limits.BasisCharge, CostOwnerID: f.owner,
		DailyWindowID: f.windows.DailyID, DailyAccrued: "0",
		MonthlyWindowID: f.windows.MonthlyID, MonthlyAccrued: "0",
	}); err != nil {
		t.Fatalf("ApplyCostSnapshot: %v", err)
	}
	pending, _ := f.keys()
	chargePending := strings.Replace(pending, ":cost:pending", ":charge:pending", 1)

	chargeRequest := limits.Request{
		CostOwnerID: f.owner, LookupID: limLookup(),
		DailyCostLimit: limPointer("10"), CostBasis: limits.BasisCharge,
		LeaseTTL: 5 * time.Second, CostEstimate: "0.5", RequestID: uuid.NewString(),
	}
	if _, err := f.limiter.Reserve(t.Context(), chargeRequest); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if got := limHashOrNone(t, f.c, chargePending)["sum"]; got != "0.5" {
		t.Fatalf("charge pending = %q, want 0.5", got)
	}
	if got := limHashOrNone(t, f.c, pending)["sum"]; got != "" && got != "0" {
		t.Fatalf("cost pending = %q, want nothing held", got)
	}
	windows := f.windows
	if _, _, err := f.limiter.ApplyCostSnapshot(t.Context(), limits.CostSnapshot{
		Basis: limits.BasisCharge, CostOwnerID: f.owner,
		DailyWindowID: windows.DailyID, DailyAccrued: "0.5",
		MonthlyWindowID: windows.MonthlyID, MonthlyAccrued: "0.5",
		RequestID: chargeRequest.RequestID,
	}); err != nil {
		t.Fatalf("ApplyCostSnapshot: %v", err)
	}
	if got := limHashOrNone(t, f.c, chargePending)["sum"]; got != "" && got != "0" {
		t.Fatalf("charge pending after the snapshot = %q, want released", got)
	}
}
