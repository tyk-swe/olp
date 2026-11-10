package reconcile

import (
	"context"
	"errors"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/limits"
)

const (
	errorAdapterUnavailable    batches.ErrorClass = "batch_adapter_unavailable"
	errorRuntimeUnavailable    batches.ErrorClass = "provider_resource_runtime_unavailable"
	errorCredentialUnavailable batches.ErrorClass = "provider_resource_credential_unavailable"
	errorSubmissionInterrupted batches.ErrorClass = "submission_interrupted"
	errorSubmissionConflict    batches.ErrorClass = "submission_conflict"
	errorSubmissionAmbiguous   batches.ErrorClass = "submission_ambiguous"
	errorAdoptionUnavailable   batches.ErrorClass = "submission_adoption_unavailable"
	errorReconcileUnavailable  batches.ErrorClass = "submission_reconcile_unavailable"
	errorPollUnavailable       batches.ErrorClass = "batch_poll_unavailable"
	errorResultReadUnavailable batches.ErrorClass = "batch_result_read_unavailable"
	errorCheckpointUnavailable batches.ErrorClass = "batch_checkpoint_unavailable"
	errorCloseUnavailable      batches.ErrorClass = "batch_close_unavailable"
	errorResultConflict        batches.ErrorClass = "result_conflict"
	errorResultTooLarge        batches.ErrorClass = "batch_result_too_large"
	errorStorageScope          batches.ErrorClass = "storage_scope"
)

func (w *Worker) failure(ctx context.Context, settlement batches.Settlement, err error, fallback batches.ErrorClass) outcome {
	switch {
	case errors.Is(err, upstream.ErrProviderConflict), errors.Is(err, batches.ErrCursorConflict), errors.Is(err, batches.ErrResultConflict):
		return w.conflict(ctx, settlement)
	case errors.Is(err, batches.ErrBatchTooLarge), errors.Is(err, batches.ErrLineTooLarge):
		return w.closeUncertain(ctx, settlement, errorResultTooLarge)
	case errors.Is(err, upstream.ErrStorageScope):
		return w.closeUncertain(ctx, settlement, errorStorageScope)
	default:
		return w.failOrClose(ctx, settlement, fallback)
	}
}

func (w *Worker) expired(settlement batches.Settlement) bool {
	return !w.deps.Now().Before(settlement.CloseAfter)
}

func (w *Worker) conflict(ctx context.Context, settlement batches.Settlement) outcome {
	if w.expired(settlement) {
		return w.closeUncertain(ctx, settlement, errorResultConflict)
	}
	return w.finish(ctx, settlement, batches.StateConflict, settlement.CloseAfter, errorResultConflict)
}

func (w *Worker) failOrClose(ctx context.Context, settlement batches.Settlement, class batches.ErrorClass) outcome {
	if w.expired(settlement) {
		return w.closeUncertain(ctx, settlement, "")
	}
	return w.retry(ctx, settlement, settlement.State, class)
}

func (w *Worker) retry(ctx context.Context, settlement batches.Settlement, state batches.State, class batches.ErrorClass) outcome {
	result := w.finish(ctx, settlement, state, w.deps.Now().Add(errorBackoff(settlement.Attempts)), class)
	if result != outcomeProgress || class == "" {
		return result
	}
	w.deps.Log.Warn("batch settlement will retry", "resource_id", settlement.ResourceID, "error_class", class)
	return outcomeFailed
}

func (w *Worker) poll(ctx context.Context, settlement batches.Settlement, state batches.State, attempts int) outcome {
	return w.finish(ctx, settlement, state, w.deps.Now().Add(pollBackoff(attempts)), "")
}

func (w *Worker) finish(ctx context.Context, settlement batches.Settlement, state batches.State, next time.Time, class batches.ErrorClass) outcome {
	if err := w.deps.Store.Finish(ctx, settlement, state, next, class); err != nil {
		w.logFailure(settlement, state, class, err)
		return outcomeFailed
	}
	return outcomeProgress
}

func (w *Worker) closeUncertain(ctx context.Context, settlement batches.Settlement, class batches.ErrorClass) outcome {
	counts, err := w.deps.Store.ResultCounts(ctx, settlement.ResourceID)
	if err != nil {
		w.logFailure(settlement, batches.StateUncertain, errorCloseUnavailable, err)
		return outcomeFailed
	}
	complete := batches.CloseDecision(settlement.ExpectedResults, counts, false, false, w.deps.Now(), settlement.CloseAfter)
	if class == "" && complete.State == batches.StateSettled {
		return w.close(ctx, settlement, complete, "")
	}
	return w.close(ctx, settlement, batches.Decision{State: batches.StateUncertain, Gap: batches.Gap(settlement.ExpectedResults, counts)}, class)
}

func (w *Worker) close(ctx context.Context, settlement batches.Settlement, decision batches.Decision, class batches.ErrorClass) outcome {
	snapshots, err := w.deps.Store.Close(ctx, settlement, batches.Closing{Decision: decision, GapDigest: w.gapDigest, ErrorClass: class}, w.deps.Now())
	if err != nil {
		w.logFailure(settlement, decision.State, errorCloseUnavailable, err)
		return outcomeFailed
	}
	w.applySnapshots(ctx, snapshots)
	if decision.State == batches.StateUncertain {
		return outcomeUncertain
	}
	return outcomeProgress
}

func (w *Worker) applySnapshots(ctx context.Context, snapshots []limits.Snapshot) {
	if err := w.deps.Limiter.InstallSnapshots(ctx, snapshots); err != nil {
		w.deps.Log.Warn("batch cost snapshot application failed; reconciliation will repair it", "error", err)
	}
}

func (w *Worker) logFailure(settlement batches.Settlement, state batches.State, class batches.ErrorClass, err error) {
	w.deps.Log.Error("batch settlement checkpoint failed", "resource_id", settlement.ResourceID,
		"state", state, "error_class", class, "error", err)
}

func pollBackoff(attempts int) time.Duration {
	return min(30*time.Second<<max(min(attempts-1, 9), 0), 10*time.Minute)
}

func errorBackoff(attempts int) time.Duration {
	return min(5*time.Second<<max(min(attempts-1, 6), 0), 5*time.Minute)
}
