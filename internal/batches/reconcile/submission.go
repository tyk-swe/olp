package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
)

var errStrictEvidence = errors.New("strict batch reconciliation evidence is invalid")

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
	put, err := w.adoptedResource(ctx, settlement, snapshot, adoption)
	if err != nil {
		return w.failOrClose(ctx, settlement, errorAdoptionUnavailable)
	}
	expected := adoptedExpected(settlement, snapshot)
	err = w.deps.Store.Adopt(ctx, settlement, snapshot.ID, expected, put)
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

func (w *Worker) adoptedResource(ctx context.Context, settlement batches.Settlement, snapshot upstream.Snapshot, adoption upstream.Adoption) (func(context.Context, pgx.Tx) error, error) {
	resource := settlement.Binding.Resource(settlement.ResourceID, snapshot.ID)
	if settlement.Binding.Kind != resources.KindStrictBatch {
		resource.State, resource.Metadata = adoption.State, adoption.Metadata
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := w.deps.Resources.PutTx(ctx, tx, &resource)
			return err
		}, nil
	}
	payload, err := w.strictContract(ctx, settlement, snapshot)
	if err != nil {
		return nil, err
	}
	metadata, err := json.Marshal(map[string]string{"upstream_model": settlement.Model()})
	if err != nil {
		return nil, err
	}
	version := resources.DurableContractVersion
	expires := settlement.CreatedAt.Add(resources.DurableLifetime - time.Second)
	resource.State, resource.Metadata = adoption.State, metadata
	resource.ContractVersion, resource.ExpiresAt = &version, &expires
	return func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.deps.Resources.PutRecoveredDurableContractTx(ctx, tx, &resource, payload)
		return err
	}, nil
}

func (w *Worker) strictContract(ctx context.Context, settlement batches.Settlement, snapshot upstream.Snapshot) ([]byte, error) {
	submission, ok := settlement.Submission.(batches.FilesSubmission)
	if !ok || snapshot.ID == "" || snapshot.InputFileID != submission.InputFileID || wire.BatchStageOf(snapshot.Status) == wire.BatchUnknown {
		return nil, errStrictEvidence
	}
	staged, err := w.deps.Resources.ReadStagedDurableContract(ctx, settlement.ResourceID)
	if err != nil {
		return nil, err
	}
	var document map[string]json.RawMessage
	if err = json.Unmarshal(staged, &document); err != nil {
		return nil, err
	}
	var effective []byte
	if err = json.Unmarshal(document["effective"], &effective); err != nil {
		return nil, err
	}
	var request struct {
		InputFileID string `json:"input_file_id"`
	}
	if err = json.Unmarshal(effective, &request); err != nil || request.InputFileID != submission.InputFileID {
		return nil, errStrictEvidence
	}
	result, err := json.Marshal(snapshot.Body)
	if err != nil {
		return nil, err
	}
	merged := maps.Clone(document)
	merged["result"] = result
	return json.Marshal(merged)
}
