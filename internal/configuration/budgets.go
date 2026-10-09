package configuration

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
)

func normalizedBudget(policy *access.BudgetPolicy) *access.BudgetPolicy {
	if !policy.Limited() {
		return nil
	}
	return policy
}
func applyInstallationBudget(ctx context.Context, tx pgx.Tx, p access.Principal, desired *access.BudgetPolicy) (bool, error) {
	if desired == nil {
		return false, nil
	}
	desired = normalizedBudget(desired)
	var id string
	var current *access.BudgetPolicy
	if err := tx.QueryRow(ctx, "SELECT id::text,budget_policy FROM olp.installation WHERE singleton FOR UPDATE").Scan(&id, &current); err != nil {
		return false, err
	}
	if reflect.DeepEqual(current, desired) {
		return false, nil
	}
	if err := p.Authorize(access.Settings); err != nil {
		return false, err
	}
	if desired.Limited() {
		if err := limits.EnsureAggregateBudget(ctx, tx, "installation", id); err != nil {
			return false, err
		}
	}
	encoded, err := json.Marshal(desired)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, "UPDATE olp.installation SET budget_policy=NULLIF($1::jsonb,'null'::jsonb),budget_etag=$2 WHERE singleton", encoded, access.NewID())
	return err == nil, err
}

func applyBudgetTimeZone(ctx context.Context, tx pgx.Tx, p access.Principal, desired *string) error {
	if desired == nil {
		return nil
	}
	var current string
	if err := tx.QueryRow(ctx, "SELECT value FROM olp.settings WHERE key='budgets.time_zone' FOR UPDATE").Scan(&current); err != nil {
		return err
	}
	if current == *desired {
		return nil
	}
	if err := p.Authorize(access.Settings); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SELECT olp.schedule_budget_zone($1,now())", *desired); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE olp.settings SET value=$1,etag=$2,updated_by=$3,updated_at=now() WHERE key='budgets.time_zone'", *desired, access.NewID(), p.UserID())
	return err
}
