package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	budgetAlertLockID   = int64(0x4f4c505f414c5254)
	notificationEvery   = time.Minute
	notificationTimeout = 2 * time.Minute

	maxDeliveryAttempts = 5

	deliveriesPerPass = 200
)

func windowColumn(column string) string {
	return `CASE r.window_kind WHEN 'day' THEN COALESCE(kwd.` + column + `,gwd.` + column + `)
      WHEN 'week' THEN COALESCE(kww.` + column + `,gww.` + column + `) ELSE COALESCE(kwm.` + column + `,gwm.` + column + `) END`
}

var dueAlertSQL = `SELECT r.id::text,r.threshold_percent,r.window_kind,
 CASE r.subject_kind WHEN 'api_key' THEN k.id ELSE g.id END::text AS subject_id,
 ` + windowColumn("window_id") + ` AS window_id,
 COALESCE(CASE WHEN COALESCE(k.effective_limits,g.effective_limits)->>'cost_basis'='charge' THEN ` + windowColumn("charged") + `
 ELSE ` + windowColumn("accrued") + ` END,0)::text AS accrued,
 COALESCE(k.effective_limits,g.effective_limits)->>(CASE r.window_kind WHEN 'day' THEN 'daily' WHEN 'week' THEN 'weekly' ELSE 'monthly' END||'_cost_limit') AS limit
FROM olp.notification_rules r
LEFT JOIN olp.api_keys_with_limits k ON r.subject_kind='api_key' AND k.id=r.subject_id
LEFT JOIN olp.budget_groups_with_limits g ON r.subject_kind='budget_group' AND g.id=r.subject_id
LEFT JOIN olp.api_key_cost_windows kww ON kww.api_key_id=k.id AND kww.window_kind='week' AND kww.weekly_complete
LEFT JOIN olp.budget_group_cost_windows gww ON gww.budget_group_id=g.id AND gww.window_kind='week' AND gww.weekly_complete
LEFT JOIN olp.api_key_cost_windows kwd ON kwd.api_key_id=k.id AND kwd.window_kind='day'
LEFT JOIN olp.api_key_cost_windows kwm ON kwm.api_key_id=k.id AND kwm.window_kind='month'
LEFT JOIN olp.budget_group_cost_windows gwd ON gwd.budget_group_id=g.id AND gwd.window_kind='day'
LEFT JOIN olp.budget_group_cost_windows gwm ON gwm.budget_group_id=g.id AND gwm.window_kind='month'
JOIN olp.notification_destinations d ON d.id=r.destination_id
WHERE r.event=$1 AND r.enabled AND d.enabled`

const insertBudgetDeliverySQL = `INSERT INTO olp.notification_deliveries (id, rule_id, window_id, threshold_percent, accrued, limit_amount, currency, status)
 VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, $7, 'pending') ON CONFLICT DO NOTHING`

type dueAlert struct {
	ruleID     string
	threshold  int
	windowKind contract.BudgetWindowKind
	subjectID  *string
	windowID   *int64
	accrued    string
	limit      *string
}

func (a dueAlert) inCurrentWindow(windows limits.Windows) bool {
	if a.windowID == nil {
		return false
	}
	switch a.windowKind {
	case contract.BudgetWindowKindDay:
		return *a.windowID == windows.DailyID
	case contract.BudgetWindowKindWeek:
		return *a.windowID == windows.WeeklyID
	case contract.BudgetWindowKindMonth:
		return *a.windowID == windows.MonthlyID
	}
	return false
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

type DeliveryConfig struct {
	Pool         *pgxpool.Pool
	Keys         *secrets.KeyRing
	Installation string
	Sender       *Sender
	Log          *slog.Logger
	Limiter      *limits.Limiter
	Catalog      *catalog.Signed
	NewID        func() string
	Now          func() time.Time
}

type deliveryWorker struct{ DeliveryConfig }

func RunDelivery(ctx context.Context, config DeliveryConfig) {
	w := &deliveryWorker{config}
	ticker := time.NewTicker(notificationEvery)
	defer ticker.Stop()
	for ctx.Err() == nil {
		outcome, progress := w.pass(ctx)
		if ctx.Err() != nil {
			return
		}
		if err := workerhealth.New(workerhealth.TaskNotificationDelivery, "").Checkpoint(ctx, w.Pool, outcome, progress); err != nil {
			w.Log.Warn("notification delivery checkpoint failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

var errLockNotHeld = errors.New("another replica holds the budget alert lock")

func (w *deliveryWorker) pass(ctx context.Context) (workerhealth.Outcome, bool) {
	passCtx, cancel := context.WithTimeout(ctx, notificationTimeout)
	defer cancel()
	claimed, err := w.claimDue(passCtx)
	if errors.Is(err, errLockNotHeld) {
		return workerhealth.OutcomeSkipped, false
	}
	if err != nil {
		w.Log.Warn("budget alert evaluation failed", "error", err)
		return workerhealth.OutcomeFailure, false
	}
	deliveries, err := w.pending(passCtx)
	if err != nil {
		w.Log.Warn("pending notification query failed", "error", err)
		return workerhealth.OutcomeFailure, claimed > 0
	}
	sent := 0
	for _, d := range deliveries {
		if passCtx.Err() != nil {
			break
		}
		if w.deliver(passCtx, d) {
			sent++
		}
	}
	return workerhealth.OutcomeSuccess, claimed > 0 || sent > 0
}

func (w *deliveryWorker) claimDue(ctx context.Context) (int, error) {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var held bool
	if err = tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", budgetAlertLockID).Scan(&held); err != nil {
		return 0, err
	}
	if !held {
		return 0, errLockNotHeld
	}
	budgets, err := w.claimBudgetAlerts(ctx, tx)
	if err != nil {
		return 0, err
	}
	reminders, err := claimKeyReminders(ctx, tx)
	if err != nil {
		return 0, err
	}
	signals, err := w.claimSignals(ctx, tx)
	if err != nil {
		return 0, err
	}
	return budgets + reminders + signals, tx.Commit(ctx)
}

func (w *deliveryWorker) claimBudgetAlerts(ctx context.Context, tx pgx.Tx) (int, error) {
	rows, err := tx.Query(ctx, dueAlertSQL, contract.NotificationEventBudgetThreshold)
	if err != nil {
		return 0, err
	}
	alerts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (dueAlert, error) {
		var a dueAlert
		err := row.Scan(&a.ruleID, &a.threshold, &a.windowKind, &a.subjectID, &a.windowID, &a.accrued, &a.limit)
		return a, err
	})
	if err != nil {
		return 0, err
	}
	windows, err := limits.CurrentBudgetWindows(ctx, tx, w.Now())
	if err != nil {
		return 0, err
	}
	currency := w.currency(ctx, tx)
	claimed := 0
	for _, a := range alerts {
		if !a.inCurrentWindow(windows) {
			continue
		}
		accrued, limit, met := thresholdEvidence(a.accrued, a.limit, a.threshold)
		if !met {
			continue
		}
		tag, err := tx.Exec(ctx, insertBudgetDeliverySQL,
			w.NewID(), a.ruleID, *a.windowID, a.threshold, accrued.String(), limit.String(), currency)
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

const subscribedRulesSQL = `SELECT r.id::text FROM olp.notification_rules r
 JOIN olp.notification_destinations d ON d.id=r.destination_id WHERE r.event=$1 AND r.enabled AND d.enabled`

const insertGrantLapseDeliverySQL = `INSERT INTO olp.notification_deliveries (id, rule_id, credential_id, payload, status)
 VALUES ($1, $2, $3, $4, 'pending') ON CONFLICT DO NOTHING`

func NotifyGrantLapsed(ctx context.Context, tx pgx.Tx, lapse GrantLapse) error {
	rows, err := tx.Query(ctx, subscribedRulesSQL, contract.NotificationEventProviderGrantLapsed)
	if err != nil {
		return err
	}
	rules, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(rules) == 0 {
		return err
	}
	payload, err := json.Marshal(lapse)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if _, err = tx.Exec(ctx, insertGrantLapseDeliverySQL, access.NewID(), rule, lapse.CredentialID, payload); err != nil {
			return err
		}
	}
	return nil
}
