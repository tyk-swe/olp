//go:build integration

package integration_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/limits"
)

func TestWeeklyBudgetAdmissionNeedsCompleteCurrentEvidence(t *testing.T) {
	client := limClient(t)
	namespace := limNamespace(t, client, "weekly")
	limiter := limLimiter(t, client, namespace)
	w := limits.BudgetWindows(time.Now())
	owner := uuid.NewString()
	request := limRequest(limLookup())
	request.CostOwnerID = owner
	request.WeeklyCostLimit = limPointer("10")
	request.CostEstimate = "2"
	request.RequestID = uuid.NewString()
	if _, err := limiter.Reserve(t.Context(), request); !errors.Is(err, limits.ErrUninitializedCost) {
		t.Fatalf("unknown weekly spend: %v", err)
	}
	snapshot := limits.CostSnapshot{CostOwnerID: owner, DailyWindowID: w.DailyID, DailyAccrued: "0", MonthlyWindowID: w.MonthlyID, MonthlyAccrued: "0", WeeklyWindowID: w.WeeklyID - 1, WeeklyAccrued: "0"}
	if _, _, err := limiter.ApplyCostSnapshot(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := limiter.Reserve(t.Context(), request); !errors.Is(err, limits.ErrUninitializedCost) {
		t.Fatalf("stale weekly spend: %v", err)
	}
	snapshot.WeeklyWindowID = w.WeeklyID
	snapshot.WeeklyAccrued = "7"
	if _, _, err := limiter.ApplyCostSnapshot(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	lease, err := limiter.Reserve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.RequestID = uuid.NewString()
	_, err = limiter.Reserve(t.Context(), request)
	var exceeded *limits.ExceededError
	if !errors.As(err, &exceeded) || exceeded.Dimension != limits.DimensionWeeklyCost {
		t.Fatalf("concurrent hold did not enforce week: %v", err)
	}
	if err = lease.Refund(t.Context()); err != nil {
		t.Fatal(err)
	}
	request.CostEstimate = "3"
	lease, err = limiter.Reserve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	lease.SetActualCost("3", "3")
	if err = lease.SettleCost(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot.RequestID = request.RequestID
	snapshot.WeeklyAccrued = "10"
	if _, _, err = limiter.ApplyCostSnapshot(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err = lease.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	request.RequestID = uuid.NewString()
	request.CostEstimate = "0.1"
	_, err = limiter.Reserve(t.Context(), request)
	if !errors.As(err, &exceeded) || exceeded.Dimension != limits.DimensionWeeklyCost {
		t.Fatalf("weekly accrued exhaustion: %v", err)
	}
}
