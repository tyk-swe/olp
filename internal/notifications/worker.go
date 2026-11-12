package notifications

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	notificationEvery   = time.Minute
	notificationTimeout = 2 * time.Minute

	maxDeliveryAttempts = 5

	deliveriesPerPass = 200
)

type DeliveryConfig struct {
	Pool         *pgxpool.Pool
	Keys         *secrets.KeyRing
	Installation string
	Sender       *Sender
	Log          *slog.Logger
	Fleet        *fleet.State
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
	if err := tx.QueryRow(ctx, tryBudgetAlertLockSQL, budgetAlertLockKey).Scan(&held); err != nil {
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

const (
	budgetAlertLockKey   = int64(0x4f4c505f414c5254)
	incidentLockSeed     = int64(502)
	ruleCreationLockName = "olp.notification_rule_creation"

	tryBudgetAlertLockSQL = `SELECT pg_try_advisory_xact_lock($1)`
	tryIncidentLockSQL    = `SELECT pg_try_advisory_lock(hashtextextended($1,$2))`
	unlockIncidentSQL     = `SELECT pg_advisory_unlock(hashtextextended($1,$2))`
	ruleCreationLockSQL   = `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`
)
