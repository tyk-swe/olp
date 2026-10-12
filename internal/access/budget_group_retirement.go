package access

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Retirement preserves the original identity carried by in-flight events and
// accepted generation templates without retaining assignable budget policy.
// The caller owns the serialized, authorized conditional mutation transaction.
func retireBudgetGroup(ctx context.Context, tx pgx.Tx, id string) error {
	var used bool
	if err := tx.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM olp.api_keys WHERE budget_group_id=$1)
 OR EXISTS(SELECT 1 FROM olp.notification_rules WHERE subject_kind='budget_group' AND subject_id=$1)
 OR EXISTS(SELECT 1 FROM olp.requests WHERE budget_group_id=$1)
 OR EXISTS(SELECT 1 FROM olp.attempt_usage_facts WHERE budget_group_id=$1)
 OR EXISTS(SELECT 1 FROM olp.attempt_usage_hourly WHERE budget_group_id=$1)
 OR EXISTS(SELECT 1 FROM olp.budget_group_cost_windows WHERE budget_group_id=$1)`, id).Scan(&used); err != nil {
		return err
	}
	if used {
		return Fail(409, "budget_group_in_use", "The budget group still has dependent resources or retained history.")
	}
	_, err := tx.Exec(ctx, `UPDATE olp.budget_groups SET retired_at=now(),updated_at=now(),etag=$2,
 limit_template=NULL,requests_per_minute=NULL,tokens_per_minute=NULL,max_concurrency=NULL,
 daily_cost_limit=NULL,weekly_cost_limit=NULL,monthly_cost_limit=NULL WHERE id=$1`, id, NewID())
	return err
}
