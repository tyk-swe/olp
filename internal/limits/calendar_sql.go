package limits

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// CurrentBudgetWindows shares the database calendar used by reconciliation and
// management reporting. It is for control/worker paths, never hot-path admission.
func CurrentBudgetWindows(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, at time.Time) (Windows, error) {
	var w Windows
	err := db.QueryRow(ctx, `SELECT daily_id,daily_start,daily_end,monthly_id,monthly_start,monthly_end,weekly_id,weekly_start,weekly_end FROM olp.budget_windows($1)`, at).Scan(&w.DailyID, &w.DailyStart, &w.DailyEnd, &w.MonthlyID, &w.MonthlyStart, &w.MonthlyEnd, &w.WeeklyID, &w.WeeklyStart, &w.WeeklyEnd)
	return w, err
}
