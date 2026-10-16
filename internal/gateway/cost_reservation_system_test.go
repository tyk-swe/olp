//go:build integration

package gateway

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
)

func installBudgetBalance(t *testing.T, limiter *limits.Limiter, owner string) {
	t.Helper()
	windows := limits.BudgetWindows(time.Now())
	_, _, err := limiter.ApplyCostSnapshot(t.Context(), limits.CostSnapshot{
		CostOwnerID: owner, DailyWindowID: windows.DailyID, MonthlyWindowID: windows.MonthlyID,
		DailyAccrued: "0", MonthlyAccrued: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Either leg fits alone; together they exceed the budget.

// A session admits before its price is known, so its target's price is held
// before the provider is dialled: 100 tokens at 10000 per million is 1.
