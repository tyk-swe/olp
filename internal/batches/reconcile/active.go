package reconcile

import (
	"context"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
)

func (w *Worker) reconcileActive(ctx context.Context, settlement batches.Settlement, reader upstream.Reader, target upstream.Target, snapshot upstream.Snapshot) outcome {
	expected, err := w.recordExpected(ctx, settlement, snapshot)
	if err != nil {
		return w.failOrClose(ctx, settlement, errorCheckpointUnavailable)
	}
	counted := settlement.WithExpected(expected)
	files := snapshot.OutputFileID != "" || snapshot.ErrorFileID != ""
	exhausted, progressed, err := w.readResults(ctx, counted, reader, target, snapshot, files)
	if err != nil {
		return w.failure(ctx, counted, err, errorResultReadUnavailable)
	}
	counts, err := w.deps.Store.ResultCounts(ctx, counted.ResourceID)
	if err != nil {
		return w.failOrClose(ctx, counted, errorCheckpointUnavailable)
	}
	decision := batches.CloseDecision(expected, counts, false, snapshot.Terminal && exhausted, w.deps.Now(), counted.CloseAfter)
	if decision.State == batches.StateSettled || decision.State == batches.StateUncertain {
		return w.close(ctx, counted, decision, "")
	}
	state := batches.StateActive
	if snapshot.Terminal || files {
		state = batches.StateSettling
	}
	attempts := counted.Attempts
	if progressed {
		attempts = 0
	}
	return w.poll(ctx, counted, state, attempts)
}

func (w *Worker) recordExpected(ctx context.Context, settlement batches.Settlement, snapshot upstream.Snapshot) (*int64, error) {
	provisional := snapshot.Expected == nil || !snapshot.Terminal && *snapshot.Expected == 0
	if settlement.ExpectedResults != nil || provisional {
		return settlement.ExpectedResults, nil
	}
	return snapshot.Expected, w.deps.Store.SetExpected(ctx, settlement, snapshot.Expected)
}

func (w *Worker) readResults(ctx context.Context, settlement batches.Settlement, reader upstream.Reader, target upstream.Target, snapshot upstream.Snapshot, files bool) (bool, bool, error) {
	if !files {
		return true, false, nil
	}
	progressed := false
	exhausted, err := reader.ReadResults(ctx, target, snapshot, settlement.Cursor, func(results []batches.Result, next batches.Cursor) error {
		page := make([]batches.PageResult, len(results))
		for i, result := range results {
			page[i] = batches.PageResult{CustomIDDigest: batches.CustomIDDigest(w.deps.DigestKey, result.CustomID), Result: result}
		}
		snapshots, err := w.deps.Store.CommitPage(ctx, settlement, page, next, w.deps.Now())
		w.applySnapshots(ctx, snapshots)
		progressed = progressed || len(results) > 0
		return err
	})
	return exhausted, progressed, err
}
