package notifications

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
)

const dueAlertSQL = `WITH current_windows(window_kind,window_id) AS (
 SELECT v.* FROM olp.budget_windows($2) b,LATERAL (VALUES('day',b.daily_id),('week',b.weekly_id),('month',b.monthly_id)) v
) SELECT r.id::text,r.threshold_percent,cw.window_id,
 CASE WHEN COALESCE(k.effective_limits,g.effective_limits)->>'cost_basis'='charge' THEN COALESCE(kw.charged,gw.charged)
 ELSE COALESCE(kw.accrued,gw.accrued) END::text AS accrued,
 ((COALESCE(k.effective_limits,g.effective_limits)->>(CASE r.window_kind WHEN 'day' THEN 'daily' WHEN 'week' THEN 'weekly' ELSE 'monthly' END||'_cost_limit'))::numeric
 +olp.active_budget_increase(r.subject_id,r.window_kind,cw.window_id,$2))::text AS limit
FROM olp.notification_rules r
JOIN olp.notification_destinations d ON d.id=r.destination_id
JOIN current_windows cw ON cw.window_kind=r.window_kind
LEFT JOIN olp.api_keys_with_limits k ON r.subject_kind='api_key' AND k.id=r.subject_id
LEFT JOIN olp.budget_groups_with_limits g ON r.subject_kind='budget_group' AND g.id=r.subject_id
LEFT JOIN olp.api_key_cost_windows kw ON kw.api_key_id=k.id AND kw.window_kind=cw.window_kind AND kw.window_id=cw.window_id
 AND (cw.window_kind<>'week' OR kw.weekly_complete)
LEFT JOIN olp.budget_group_cost_windows gw ON gw.budget_group_id=g.id AND gw.window_kind=cw.window_kind AND gw.window_id=cw.window_id
 AND (cw.window_kind<>'week' OR gw.weekly_complete)
WHERE r.event=$1 AND r.enabled AND d.enabled AND (kw.window_id IS NOT NULL OR gw.window_id IS NOT NULL)`

const insertBudgetDeliverySQL = `INSERT INTO olp.notification_deliveries (id, rule_id, window_id, threshold_percent, accrued, limit_amount, currency, status)
 VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, $7, 'pending') ON CONFLICT DO NOTHING`

type dueAlert struct {
	ruleID    string
	threshold int
	windowID  int64
	accrued   string
	limit     *string
}

func thresholdEvidence(accrued string, limit *string, threshold int) (decimal.Decimal, decimal.Decimal, bool) {
	if limit == nil {
		return decimal.Decimal{}, decimal.Decimal{}, false
	}
	limitValue, err := decimal.NewFromString(*limit)
	if err != nil || !limitValue.IsPositive() {
		return decimal.Decimal{}, decimal.Decimal{}, false
	}
	accruedValue, err := decimal.NewFromString(accrued)
	if err != nil || accruedValue.IsNegative() {
		return decimal.Decimal{}, decimal.Decimal{}, false
	}
	return accruedValue, limitValue, accruedValue.Mul(decimal.NewFromInt(100)).
		Cmp(limitValue.Mul(decimal.NewFromInt(int64(threshold)))) >= 0
}

func (w *deliveryWorker) claimBudgetAlerts(ctx context.Context, tx pgx.Tx) (int, error) {
	rows, err := tx.Query(ctx, dueAlertSQL, contract.NotificationEventBudgetThreshold, w.Now())
	if err != nil {
		return 0, err
	}
	alerts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (dueAlert, error) {
		var a dueAlert
		err := row.Scan(&a.ruleID, &a.threshold, &a.windowID, &a.accrued, &a.limit)
		return a, err
	})
	if err != nil {
		return 0, err
	}
	currency := w.currency(ctx, tx)
	claimed := 0
	for _, a := range alerts {
		accrued, limit, met := thresholdEvidence(a.accrued, a.limit, a.threshold)
		if !met {
			continue
		}
		tag, err := tx.Exec(ctx, insertBudgetDeliverySQL,
			w.NewID(), a.ruleID, a.windowID, a.threshold, accrued.String(), limit.String(), currency)
		if err != nil {
			return 0, err
		}
		claimed += int(tag.RowsAffected())
	}
	return claimed, nil
}

func (w *deliveryWorker) currency(ctx context.Context, q access.Queryer) *string {
	var currency string
	err := q.QueryRow(ctx, notificationCurrencySQL).Scan(&currency)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		w.Log.Warn("budget alert currency lookup failed", "error", err)
	}
	if currency == "" {
		return nil
	}
	return &currency
}

const notificationCurrencySQL = `SELECT currency FROM olp.pricing_currency WHERE singleton`
