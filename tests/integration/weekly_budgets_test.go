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
	lease.SetActualCost("3")
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

func TestWeeklyKeyAndGroupBudgetsEnforceAndReportIndependently(t *testing.T) {
	f := glSeedIn(t, "weekly-key-group", glPrice{input: "1000000", output: "1000000"})
	h := f.h
	group := h.want(f.owner, "POST", "/api/v1/budget-groups", map[string]any{"name": "Weekly group", "weekly_cost_limit": "1000"}, idem("weekly-group"), 201)
	key, secret := f.key("Weekly key", map[string]any{"weekly_cost_limit": "1000", "budget_group_id": group["id"]})
	conn, err := h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := limits.ReconciliationSnapshots(t.Context(), conn.Conn(), time.Now())
	conn.Release()
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range snapshots {
		if _, _, err = f.limiter.ApplyCostSnapshot(t.Context(), snapshot); err != nil {
			t.Fatal(err)
		}
	}
	await := endUserAccounting(t, h, f.limiter)
	if status, err := endUserChat(t.Context(), h, secret, routeSlug, "unused"); err != nil || status != 200 {
		t.Fatalf("weekly key/group admission: %d %v", status, err)
	}
	await()
	kp := "/api/v1/api-keys/" + key
	gp := "/api/v1/budget-groups/" + group["id"].(string)
	for _, path := range []string{kp, gp} {
		detail := h.want(f.owner, "GET", path, nil, nil, 200)
		week := detail["budget"].(map[string]any)["weekly"].(map[string]any)
		if week["accrued"] != "10.000000000000" {
			t.Fatalf("weekly report: %v", week)
		}
	}
	set := func(path, cap string) {
		detail := h.want(f.owner, "GET", path, nil, nil, 200)
		h.want(f.owner, "PATCH", path, map[string]any{"weekly_cost_limit": cap}, etagHeader(detail), 200)
		h.refresh()
	}
	set(kp, "10")
	if status, err := endUserChat(t.Context(), h, secret, routeSlug, "unused"); err != nil || status != 429 {
		t.Fatalf("weekly key cap: %d %v", status, err)
	}
	await()
	set(kp, "1000")
	set(gp, "10")
	if status, err := endUserChat(t.Context(), h, secret, routeSlug, "unused"); err != nil || status != 429 {
		t.Fatalf("weekly group cap: %d %v", status, err)
	}
	await()
	if got := h.Gateway.Admission.BudgetRejections(limits.DimensionWeeklyCost); got != 2 {
		t.Fatalf("weekly rejection metric: %d", got)
	}
}
