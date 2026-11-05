package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/usage"
)

func runDebitReconciliation(ctx context.Context, pool *pgxpool.Pool, limiter *limits.Limiter, log *slog.Logger) {
	store := &limits.DebitStore{Pool: pool, Limiter: limiter}
	go recoverWorkflowModels(ctx, store, log)
	toolLedger := accounting.NewDurableLedger(pool, store, nil)
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		pass, cancel := context.WithTimeout(ctx, 40*time.Second)
		reconcileErr := reconcileDebitPass(pass, store, toolLedger)
		cancel()
		expiry, expiryCancel := context.WithTimeout(ctx, 5*time.Second)
		_, expiryErr := toolLedger.DrainExpiredCallIdentities(expiry, 256)
		expiryCancel()
		err := errors.Join(reconcileErr, expiryErr)
		outcome := usage.OutcomeSuccess
		if err != nil {
			outcome = usage.OutcomeFailure
			log.Warn("execution debit reconciliation failed", "error", err)
		}
		checkpoint, checkpointCancel := context.WithTimeout(ctx, 5*time.Second)
		if checkpointErr := usage.CheckpointTask(checkpoint, pool, usage.TaskExecutionDebits, outcome, err == nil); checkpointErr != nil && ctx.Err() == nil {
			log.Warn("execution debit checkpoint failed", "error", checkpointErr)
		}
		checkpointCancel()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

func reconcileDebitPass(ctx context.Context, store *limits.DebitStore, toolLedger *accounting.DurableLedger) error {
	if store.Limiter == nil {
		if _, err := store.RecoverPrepared(ctx, 256); err != nil {
			return err
		}
	} else if err := store.ReconcileDebits(ctx); err != nil {
		return err
	}
	_, err := toolLedger.RecoverOrphanedDispatches(ctx, time.Now().Add(-accounting.OrphanDispatchAge), 256)
	return err
}

func recoverWorkflowModels(ctx context.Context, store *limits.DebitStore, log *slog.Logger) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		pass, cancel := context.WithTimeout(ctx, 4*time.Second)
		_, err := store.RecoverOrphanedWorkflowModels(pass, 256)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Warn("workflow model recovery failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
