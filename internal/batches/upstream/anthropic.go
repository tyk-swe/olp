package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
)

type anthropicReader struct {
	transport transport
}

type anthropicBatch struct {
	ID               string `json:"id"`
	ProcessingStatus string `json:"processing_status"`
	CreatedAt        string `json:"created_at"`
	RequestCounts    *struct {
		Processing Count `json:"processing"`
		Succeeded  Count `json:"succeeded"`
		Errored    Count `json:"errored"`
		Canceled   Count `json:"canceled"`
		Expired    Count `json:"expired"`
	} `json:"request_counts"`
}

func (r anthropicReader) Poll(ctx context.Context, settlement batches.Settlement, target Target) (Snapshot, error) {
	if settlement.UpstreamID == "" {
		return Snapshot{}, errors.New("batch upstream identity is unavailable")
	}
	body, err := r.transport.getResource(ctx, target, "messages/batches/"+url.PathEscape(settlement.UpstreamID), nil, objectLimit)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot, err := decodeAnthropicBatch(body)
	if err != nil {
		return Snapshot{}, err
	}
	if snapshot.ID != settlement.UpstreamID {
		return Snapshot{}, ErrProviderConflict
	}
	return snapshot, nil
}

func decodeAnthropicBatch(body []byte) (Snapshot, error) {
	var batch anthropicBatch
	if json.Unmarshal(body, &batch) != nil || batch.ID == "" {
		return Snapshot{}, errors.New("provider Anthropic batch object is malformed")
	}
	switch batch.ProcessingStatus {
	case "in_progress", "canceling", "ended":
	default:
		return Snapshot{}, ErrProviderConflict
	}
	createdAt, err := time.Parse(time.RFC3339Nano, batch.CreatedAt)
	if err != nil {
		return Snapshot{}, errors.New("provider Anthropic batch creation time is malformed")
	}
	expected, err := anthropicExpected(batch)
	if err != nil {
		return Snapshot{}, err
	}
	terminal := batch.ProcessingStatus == "ended"
	output := ""
	if terminal {
		output = batch.ID
	}
	return Snapshot{
		ID: batch.ID, Status: batch.ProcessingStatus, CreatedAt: createdAt, Expected: expected,
		OutputFileID: output, Body: body, Terminal: terminal,
	}, nil
}

func anthropicExpected(batch anthropicBatch) (*int64, error) {
	if batch.RequestCounts == nil {
		return nil, nil
	}
	counts := batch.RequestCounts
	total, err := sumCounts(counts.Processing, counts.Succeeded, counts.Errored, counts.Canceled, counts.Expired)
	if err != nil {
		return nil, err
	}
	return &total, nil
}

func (anthropicReader) Reconcile(context.Context, batches.Settlement, Target, KnownBatch) ([]Snapshot, error) {
	return nil, ErrNoEvidence
}

func (anthropicReader) Adopt(batches.Settlement, Snapshot) (Adoption, error) {
	return Adoption{}, ErrNoEvidence
}

func (r anthropicReader) ReadResults(ctx context.Context, target Target, snapshot Snapshot, cursor batches.Cursor, commit PageCommit) (bool, error) {
	if cursor.Phase == batches.PhaseDone {
		return true, nil
	}
	if !snapshot.Terminal || snapshot.ID == "" {
		return false, nil
	}
	if cursor.File != "" && cursor.File != snapshot.ID {
		return false, batches.ErrCursorConflict
	}
	endpoint, err := r.transport.resourceURL(target, "messages/batches/"+url.PathEscape(snapshot.ID)+"/results", nil)
	if err != nil {
		return false, err
	}
	parse := func(line []byte, _ int64) (batches.Result, error) { return parseAnthropicLine(line, target.Model) }
	next, err := stream(ctx, r.transport, target, endpoint, r.transport.provider(target, target.Config, nil), batches.MaxBatchBytes,
		func(body io.Reader, remaining int64) (batches.Cursor, int64, error) {
			next, consumed, _, err := readPages(body, cursor, snapshot.ID, batches.PhaseOutput, parse, commit, remaining)
			return next, consumed, err
		})
	if err != nil {
		return false, err
	}
	next.Phase, next.File = batches.PhaseDone, snapshot.ID
	return true, commit(nil, next)
}

func parseAnthropicLine(line []byte, model string) (batches.Result, error) {
	canonical, err := canonicalJSON(line)
	if err != nil {
		return batches.Result{}, batches.ErrCursorConflict
	}
	var object struct {
		CustomID string `json:"custom_id"`
		Result   struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		} `json:"result"`
	}
	if json.Unmarshal(canonical, &object) != nil || object.CustomID == "" {
		return batches.Result{}, batches.ErrCursorConflict
	}
	unobserved := func(kind batches.ResultKind, class batches.ErrorClass) (batches.Result, error) {
		return batches.Result{CustomID: object.CustomID, Kind: kind, ErrorClass: class, Evidence: canonical, Usage: usage.AttemptUsage{Complete: true}}, nil
	}
	switch object.Result.Type {
	case "errored":
		return unobserved(batches.ResultErrored, "anthropic_batch_errored")
	case "canceled":
		return unobserved(batches.ResultCancelled, "anthropic_batch_cancelled")
	case "expired":
		return unobserved(batches.ResultExpired, "anthropic_batch_expired")
	case "succeeded":
		completion, err := protocols.Decode(wire.Anthropic, wire.Anthropic, object.Result.Message, model, "")
		if err != nil || completion == nil {
			return batches.Result{}, batches.ErrCursorConflict
		}
		return batches.Result{
			CustomID: object.CustomID, Kind: batches.ResultSucceeded, Evidence: canonical,
			Usage: usage.ObservedAttemptUsage(completion.Usage, completion.Usage != nil, string(wire.LineGeneration)),
		}, nil
	default:
		return batches.Result{}, batches.ErrCursorConflict
	}
}
