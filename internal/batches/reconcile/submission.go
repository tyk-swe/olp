package reconcile

import (
	"context"
	"errors"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/resources"
)

func (w *Worker) reconcileSubmission(ctx context.Context, settlement batches.Settlement, reader upstream.Reader, target upstream.Target) outcome {
	matches, err := reader.Reconcile(ctx, settlement, target, w.knownBatch(settlement))
	if errors.Is(err, upstream.ErrNoEvidence) {
		if w.expired(settlement) {
			return w.closeUncertain(ctx, settlement, "")
		}
		return w.retry(ctx, settlement, batches.StateSubmissionUnknown, "")
	}
	if err != nil {
		return w.failure(ctx, settlement, err, errorReconcileUnavailable)
	}
	switch len(matches) {
	case 0:
		if w.expired(settlement) {
			return w.closeUncertain(ctx, settlement, "")
		}
		return w.poll(ctx, settlement, batches.StateSubmissionUnknown, settlement.Attempts)
	case 1:
		return w.adopt(ctx, settlement, reader, target, matches[0])
	default:
		return w.finish(ctx, settlement, batches.StateConflict, settlement.CloseAfter, errorSubmissionConflict)
	}
}

func (w *Worker) knownBatch(settlement batches.Settlement) upstream.KnownBatch {
	return func(ctx context.Context, upstreamID string) (bool, error) {
		resource := settlement.Binding.Resource(settlement.ResourceID, upstreamID)
		_, err := w.deps.Resources.GetByUpstreamIncludingDeleted(ctx, resource.NativeIdentity())
		if errors.Is(err, resources.ErrNotFound) {
			return false, nil
		}
		return err == nil, err
	}
}

func (w *Worker) adopt(ctx context.Context, settlement batches.Settlement, reader upstream.Reader, target upstream.Target, snapshot upstream.Snapshot) outcome {
	adoption, err := reader.Adopt(settlement, snapshot)
	if err != nil {
		return w.failure(ctx, settlement, err, errorAdoptionUnavailable)
	}
	recovery, err := w.adoptedResource(ctx, settlement, snapshot, adoption)
	if err != nil {
		return w.failOrClose(ctx, settlement, errorAdoptionUnavailable)
	}
	expected := adoptedExpected(settlement, snapshot)
	err = w.deps.Store.Adopt(ctx, settlement, recovery, expected)
	if errors.Is(err, batches.ErrSubmissionAmbiguous) {
		return w.failOrClose(ctx, settlement, errorSubmissionAmbiguous)
	}
	if err != nil {
		return w.failOrClose(ctx, settlement, errorAdoptionUnavailable)
	}
	return w.reconcileActive(ctx, settlement.Activated(snapshot.ID).WithExpected(expected), reader, target, snapshot)
}

func adoptedExpected(settlement batches.Settlement, snapshot upstream.Snapshot) *int64 {
	switch {
	case settlement.ExpectedResults != nil:
		return settlement.ExpectedResults
	case snapshot.Expected != nil && *snapshot.Expected == 0 && !snapshot.Terminal:
		return nil
	default:
		return snapshot.Expected
	}
}

func (w *Worker) adoptedResource(ctx context.Context, settlement batches.Settlement, snapshot upstream.Snapshot, adoption upstream.Adoption) (resources.BatchRecovery, error) {
	resource := settlement.Binding.Resource(settlement.ResourceID, snapshot.ID)
	submission, isFiles := settlement.Submission.(batches.FilesSubmission)
	if settlement.Binding.Kind == resources.KindStrictBatch && !isFiles {
		return resources.BatchRecovery{}, resources.ErrContract
	}
	return w.deps.Resources.PrepareBatchRecovery(ctx, resource, resources.BatchRecoveryEvidence{
		State: adoption.State, Metadata: adoption.Metadata, Model: settlement.Model(), CreatedAt: settlement.CreatedAt,
		SubmittedFileID: submission.InputFileID, ObservedFileID: snapshot.InputFileID,
		NativeStatus: snapshot.Status, Result: snapshot.Body,
	})
}
