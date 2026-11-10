package settings

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/limits/ledger"
)

func ApplyInstallationBudget(ctx context.Context, tx pgx.Tx, p access.Principal, desired *authority.BudgetPolicy) (bool, error) {
	if desired == nil {
		return false, nil
	}
	normalized := authority.NormalizedBudget(desired)
	var id string
	var current *authority.BudgetPolicy
	if err := tx.QueryRow(ctx, "SELECT id::text,budget_policy FROM olp.installation WHERE singleton FOR UPDATE").Scan(&id, &current); err != nil {
		return false, err
	}
	if reflect.DeepEqual(authority.NormalizedBudget(current), normalized) {
		return false, nil
	}
	if err := p.Authorize(access.Settings); err != nil {
		return false, err
	}
	if _, err := SetInstallationBudget(ctx, tx, id, normalized); err != nil {
		return false, err
	}
	return true, nil
}

func SetInstallationBudget(ctx context.Context, tx pgx.Tx, id string, policy *authority.BudgetPolicy) (string, error) {
	if policy.Limited() {
		if err := ledger.EnsureAggregateBudget(ctx, tx, "installation", id); err != nil {
			return "", err
		}
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	etag := access.NewID()
	if _, err := tx.Exec(ctx, "UPDATE olp.installation SET budget_policy=NULLIF($2::jsonb,'null'::jsonb),budget_etag=$3 WHERE id=$1", id, encoded, etag); err != nil {
		return "", err
	}
	return etag, nil
}

func ApplyBudgetTimeZone(ctx context.Context, tx pgx.Tx, p access.Principal, desired *string) error {
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

func ReadBudgetTimeZone(ctx context.Context, q access.Queryer) (string, error) {
	var zone string
	err := q.QueryRow(ctx, "SELECT COALESCE((SELECT value FROM olp.settings WHERE key='budgets.time_zone'),'UTC')").Scan(&zone)
	return zone, err
}

func ReadInstallationBudget(ctx context.Context, q access.Queryer) (*authority.BudgetPolicy, error) {
	var policy *authority.BudgetPolicy
	err := q.QueryRow(ctx, "SELECT budget_policy FROM olp.installation WHERE singleton").Scan(&policy)
	return policy, err
}
