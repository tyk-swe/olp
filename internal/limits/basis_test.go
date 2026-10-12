package limits

import (
	"strings"
	"testing"
	"time"
)

// TestChargeBasisReservesItsOwnCounters proves the basis selects an independent
// key set: a charge-basis request spends from "{owner}:charge:*" and the cost
// counters are never asked.
func TestChargeBasisReservesItsOwnCounters(t *testing.T) {
	t.Parallel()
	server := &scripted{answer: answering(t, map[string][2]any{"reserve_cost": {costGranted, nil}})}
	limiter := &Limiter{client: server, namespace: "olp:test"}
	request := costRequest(false)
	request.CostBasis = BasisCharge
	request.CostEstimate = "0.5"
	if _, err := limiter.Reserve(t.Context(), request); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	var costKeys []string
	for _, command := range server.commands() {
		for _, arg := range command {
			if strings.Contains(arg, ":charge:") {
				costKeys = append(costKeys, arg)
			}
			if strings.Contains(arg, ":cost:") {
				t.Errorf("a charge-basis reservation touched the cost key %q", arg)
			}
		}
	}
	if !strings.HasSuffix(costKeys[0], ":charge:day") || !strings.HasSuffix(costKeys[1], ":charge:month") {
		t.Fatalf("a charge-basis reservation addressed %v, want the charge day and month", costKeys[:2])
	}
}

// TestChargeBasisSettlesItsOwnReservation proves the lease remembers the basis
// the request was reserved under and both a grown estimate and a settled actual
// are written back to it.
func TestChargeBasisSettlesItsOwnReservation(t *testing.T) {
	t.Parallel()
	server := &scripted{answer: answering(t, map[string][2]any{
		"reserve_cost": {costGranted, nil}, "grow_cost": {costGranted, nil}, "settle_cost": {costSettled, nil},
	})}
	limiter := &Limiter{client: server, namespace: "olp:test"}
	request := costRequest(false)
	request.CostBasis = BasisCharge
	lease, err := limiter.Reserve(t.Context(), request)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := lease.GrowCost(t.Context(), "0.3", "0.7", request.RequestID, time.Second); err != nil {
		t.Fatalf("GrowCost: %v", err)
	}
	lease.SetActualCost("0.25", "0.55")
	if err := lease.SettleCost(t.Context()); err != nil {
		t.Fatalf("SettleCost: %v", err)
	}
	var grown, settled string
	for _, command := range server.commands() {
		if len(command) > 1 && (command[1] == reserveCostScript.digest || command[1] == reserveCostScript.source) && !strings.Contains(command[0]+command[1], "0.25") {
			grown = strings.Join(command, " ")
		}
		if len(command) > 1 && (command[1] == settleCostScript.digest || command[1] == settleCostScript.source) {
			settled = strings.Join(command, " ")
		}
	}
	if !strings.Contains(grown, ":charge:pending") || !strings.Contains(grown, "0.7") {
		t.Fatalf("the estimate grew on %q, want the charge amount on the charge keys", grown)
	}
	if !strings.Contains(settled, ":charge:pending") || !strings.Contains(settled, "0.55") {
		t.Fatalf("the actual settled %q, want the charge amount on the charge keys", settled)
	}
}

// TestCostBasisValidationAcceptsOnlyTheTwoBases proves a request cannot carry an
// invented measure into the scripts.
func TestCostBasisValidationAcceptsOnlyTheTwoBases(t *testing.T) {
	t.Parallel()
	for _, basis := range []Basis{BasisCost, BasisCharge, ""} {
		r := costRequest(false)
		r.CostBasis = basis
		if err := r.Validate(); err != nil {
			t.Errorf("basis %q: %v", basis, err)
		}
	}
	r := costRequest(false)
	r.CostBasis = "balance"
	if err := r.Validate(); err == nil {
		t.Error("a request with cost basis \"balance\" validated")
	}
}

// TestSnapshotsInstallOnTheirOwnBasis proves a charge snapshot reaches the
// charge counters and the request reservation is removed from the same set.
func TestSnapshotsInstallOnTheirOwnBasis(t *testing.T) {
	t.Parallel()
	server := &scripted{answer: answering(t, map[string][2]any{"reconcile_cost": {reply(int64(1), int64(1), "ok", int64(1), int64(1)), nil}})}
	limiter := &Limiter{client: server, namespace: "olp:test"}
	snapshot := CostSnapshot{
		Basis: BasisCharge, CostOwnerID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607",
		DailyWindowID: 1, DailyAccrued: "0.5", MonthlyWindowID: 2, MonthlyAccrued: "0.6",
		RequestID: "0192CF87-D4AB-7F2E-A8B1-C2D3E4F50608",
	}
	if _, _, err := limiter.ApplyCostSnapshot(t.Context(), snapshot); err != nil {
		t.Fatalf("ApplyCostSnapshot: %v", err)
	}
	joined := ""
	for _, command := range server.commands() {
		joined += strings.Join(command, " ")
	}
	if !strings.Contains(joined, ":charge:day") || strings.Contains(joined, ":cost:day") {
		t.Fatalf("the snapshot installed on %q, want the charge counters", joined)
	}
}
