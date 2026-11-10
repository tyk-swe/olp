package ledger

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/limits"
)

func EnsureAggregateBudget(ctx context.Context, tx pgx.Tx, level, subject string) error {
	column, ok := map[string]string{"installation": "installation_id", "project": "project_id", "organization": "organization_id"}[level]
	if !ok {
		return errors.New("invalid aggregate budget level")
	}
	_, err := tx.Exec(ctx, "INSERT INTO olp.aggregate_budget_accounts (id,"+column+") VALUES ($1,$2) ON CONFLICT (id) DO NOTHING",
		limits.AggregateBudgetID(level, subject), subject)
	return err
}

func EnsureKeyRouteBudget(ctx context.Context, tx pgx.Tx, key, route string) error {
	_, err := tx.Exec(ctx, `INSERT INTO olp.aggregate_budget_accounts (id,api_key_id,route_slug) VALUES ($1,$2,$3) ON CONFLICT(id) DO NOTHING`, limits.KeyRouteBudgetID(key, route), key, route)
	return err
}

func EnsureAttributionBudget(ctx context.Context, tx pgx.Tx, project, key, value string) error {
	_, err := tx.Exec(ctx, `INSERT INTO olp.aggregate_budget_accounts(id,project_id,attribution_key,attribution_value) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO NOTHING`, limits.AttributionBudgetID(project, key, value), project, key, value)
	return err
}

func EnsureEndUserAccounts(ctx context.Context, tx pgx.Tx, keyID string, projectID *string, digest string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO olp.end_user_accounts (id,api_key_id,project_id,end_user_digest) VALUES ($1,$2,NULL,$3) ON CONFLICT (id) DO NOTHING`, limits.EndUserKeyID(keyID, digest), keyID, digest); err != nil {
		return err
	}
	if projectID == nil {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO olp.end_user_accounts (id,api_key_id,project_id,end_user_digest) VALUES ($1,NULL,$2,$3) ON CONFLICT (id) DO NOTHING`, limits.EndUserProjectID(*projectID, digest), *projectID, digest)
	return err
}

type SupplyBudget struct {
	OwnerID string
	Kind    string
	Daily   *string
	Monthly *string
}

func ReplaceSupplyBudgets(ctx context.Context, tx pgx.Tx, budgets []SupplyBudget) error {
	if _, err := tx.Exec(ctx, "DELETE FROM olp.supply_budgets"); err != nil {
		return fmt.Errorf("clear supply budgets: %w", err)
	}
	for _, budget := range budgets {
		if _, err := tx.Exec(ctx, `INSERT INTO olp.supply_budgets(owner_id,owner_kind,daily_cost_limit,monthly_cost_limit)
VALUES($1,$2,$3::text::numeric,$4::text::numeric) ON CONFLICT (owner_id) DO NOTHING`,
			budget.OwnerID, budget.Kind, budget.Daily, budget.Monthly); err != nil {
			return fmt.Errorf("record supply budget: %w", err)
		}
	}
	return nil
}

type window struct {
	kind string
	id   int64
}

func currentWindows(w limits.Windows) [3]window {
	return [3]window{{"day", w.DailyID}, {"week", w.WeeklyID}, {"month", w.MonthlyID}}
}

func OpenFreshOwner(ctx context.Context, tx pgx.Tx, kind limits.OwnerKind, ownerID string) error {
	now := time.Now()
	switch kind {
	case limits.OwnerAggregate, limits.OwnerEndUser:
		_, err := OwnerSnapshots(ctx, tx, kind, ownerID, now)
		return err
	case limits.OwnerSupply:
		return openSupplyWindows(ctx, tx, ownerID, now)
	case limits.OwnerKey:
		return openOwnerWindows(ctx, tx, keyWindows, ownerID, now, true)
	case limits.OwnerGroup:
		return openOwnerWindows(ctx, tx, groupWindows, ownerID, now, false)
	}
	return limits.ErrUninitializedCost
}

func openSupplyWindows(ctx context.Context, tx pgx.Tx, ownerID string, now time.Time) error {
	w, err := CurrentBudgetWindows(ctx, tx, now)
	if err != nil {
		return err
	}
	for _, window := range currentWindows(w) {
		if _, err := tx.Exec(ctx, `INSERT INTO olp.supply_cost_windows(owner_id,window_kind,window_id,accrued,unpriced_attempts,weekly_complete) SELECT $1,$2,$3,0,0,true WHERE EXISTS(SELECT 1 FROM olp.supply_budgets WHERE owner_id=$1) ON CONFLICT(owner_id,window_kind,window_id) DO NOTHING`, ownerID, window.kind, window.id); err != nil {
			return err
		}
	}
	return nil
}

func earliest(times ...time.Time) time.Time {
	return slices.MinFunc(times, time.Time.Compare)
}

func latest(times ...time.Time) time.Time {
	return slices.MaxFunc(times, time.Time.Compare)
}

func openOwnerWindows(ctx context.Context, tx pgx.Tx, s storage, ownerID string, now time.Time, includesAgentPrincipal bool) error {
	w, err := CurrentBudgetWindows(ctx, tx, now)
	if err != nil {
		return err
	}
	start, end := earliest(w.DailyStart, w.MonthlyStart, w.WeeklyStart), latest(w.DailyEnd, w.MonthlyEnd, w.WeeklyEnd)
	var hasFacts bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.attempt_usage_facts WHERE (`+s.column+`=$1 OR ($4::boolean AND agent_principal_id=$1)) AND observed_at>=$2 AND observed_at<$3) OR EXISTS(SELECT 1 FROM olp.attempt_usage_hourly WHERE (`+s.column+`=$1 OR ($4::boolean AND agent_principal_id=$1)) AND budget_bucket>=$2 AND budget_bucket<$3)`, ownerID, start, end, includesAgentPrincipal).Scan(&hasFacts); err != nil {
		return err
	}
	if hasFacts {
		return limits.ErrUninitializedCost
	}
	for _, window := range currentWindows(w) {
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.table+`(`+s.column+`,window_kind,window_id,accrued,unpriced_attempts,charged,uncharged_attempts,weekly_complete) VALUES($1,$2,$3,0,0,0,0,true) ON CONFLICT(`+s.column+`,window_kind,window_id) DO NOTHING`, ownerID, window.kind, window.id); err != nil {
			return err
		}
	}
	return nil
}
