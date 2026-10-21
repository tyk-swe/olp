package batches

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
)

type AnthropicReader struct {
	Auth   *connectors.Auth
	Egress *egress.Policy
}

func (r *AnthropicReader) Poll(ctx context.Context, settlement Settlement, target *PinnedTarget) (BatchSnapshot, error) {
	if settlement.UpstreamID == nil || *settlement.UpstreamID == "" {
		return BatchSnapshot{}, errors.New("batch upstream identity is unavailable")
	}
	path := "messages/batches/" + url.PathEscape(*settlement.UpstreamID)
	body, err := (&OpenAIReader{Auth: r.Auth, Egress: r.Egress}).getJSON(ctx, target, path, nil, 1<<20)
	if err != nil {
		return BatchSnapshot{}, err
	}
	var object struct {
		ID               string `json:"id"`
		ProcessingStatus string `json:"processing_status"`
		CreatedAt        string `json:"created_at"`
		RequestCounts    *struct {
			Processing int64 `json:"processing"`
			Succeeded  int64 `json:"succeeded"`
			Errored    int64 `json:"errored"`
			Canceled   int64 `json:"canceled"`
			Expired    int64 `json:"expired"`
		} `json:"request_counts"`
	}
	if json.Unmarshal(body, &object) != nil || object.ID == "" || object.ProcessingStatus == "" {
		return BatchSnapshot{}, errors.New("provider Anthropic batch object is malformed")
	}
	switch object.ProcessingStatus {
	case "in_progress", "canceling", "ended":
	default:
		return BatchSnapshot{}, ErrProviderConflict
	}
	if object.ID != *settlement.UpstreamID {
		return BatchSnapshot{}, ErrProviderConflict
	}
	createdAt, err := time.Parse(time.RFC3339Nano, object.CreatedAt)
	if err != nil {
		return BatchSnapshot{}, errors.New("provider Anthropic batch creation time is malformed")
	}
	snapshot := BatchSnapshot{
		ID: object.ID, Status: object.ProcessingStatus, CreatedAt: createdAt,
		Body: body, Terminal: object.ProcessingStatus == "ended",
	}
	if object.RequestCounts != nil {
		counts := []int64{
			object.RequestCounts.Processing, object.RequestCounts.Succeeded,
			object.RequestCounts.Errored, object.RequestCounts.Canceled, object.RequestCounts.Expired,
		}
		var total int64
		for _, count := range counts {
			if count < 0 || count > math.MaxInt64-total {
				return BatchSnapshot{}, errors.New("provider Anthropic request counts are invalid")
			}
			total += count
		}
		snapshot.Expected = &total
	}
	if snapshot.Terminal {
		snapshot.OutputFileID = object.ID
	}
	return snapshot, nil
}

func (r *AnthropicReader) Reconcile(context.Context, Settlement, *PinnedTarget, string, string) (Reconciliation, error) {
	return Reconciliation{}, nil
}

func (r *AnthropicReader) ReadResults(ctx context.Context, target *PinnedTarget, snapshot BatchSnapshot, cursor Cursor, commit ResultPageCommit) (Cursor, bool, error) {
	if cursor.Phase == "done" {
		return cursor, true, nil
	}
	if !snapshot.Terminal || snapshot.ID == "" {
		return cursor, false, nil
	}
	if cursor.File != "" && cursor.File != snapshot.ID {
		return cursor, false, ErrCursorConflict
	}
	response, err := (&OpenAIReader{Auth: r.Auth, Egress: r.Egress}).do(
		ctx, target, http.MethodGet, "messages/batches/"+url.PathEscape(snapshot.ID)+"/results", nil, nil)
	if err != nil {
		return cursor, false, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return cursor, false, fmt.Errorf("provider Anthropic batch results returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > MaxBatchBytes {
		return cursor, false, ErrBatchTooLarge
	}
	next, _, _, err := ReadJSONLPages(response.Body, cursor, snapshot.ID, "output",
		func(line []byte) (Result, error) { return parseAnthropicResultLine(line, target.Model) }, commit)
	if err != nil {
		return next, false, err
	}
	next.Phase = "done"
	next.File = snapshot.ID
	if err := commit(nil, next); err != nil {
		return next, false, err
	}
	return next, true, nil
}

func parseAnthropicResultLine(line []byte, model string) (Result, error) {
	canonical, err := canonicalJSON(line)
	if err != nil {
		return Result{}, ErrCursorConflict
	}
	var object struct {
		CustomID string `json:"custom_id"`
		Result   struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		} `json:"result"`
	}
	if json.Unmarshal(canonical, &object) != nil || object.CustomID == "" {
		return Result{}, ErrCursorConflict
	}
	result := Result{CustomID: object.CustomID, Evidence: canonical, Usage: usage.AttemptUsage{Complete: true}}
	switch object.Result.Type {
	case "errored":
		result.Kind = ResultErrored
		result.ErrorClass = "anthropic_batch_errored"
	case "canceled":
		result.Kind = ResultCancelled
		result.ErrorClass = "anthropic_batch_cancelled"
	case "expired":
		result.Kind = ResultExpired
		result.ErrorClass = "anthropic_batch_expired"
	case "succeeded":
		completion, err := protocols.Decode(wire.Anthropic, wire.Anthropic, object.Result.Message, model, "")
		if err != nil || completion == nil {
			return Result{}, ErrCursorConflict
		}
		result.Kind = ResultSucceeded
		result.Usage = usage.ObservedAttemptUsage(completion.Usage, completion.Usage != nil, "generation")
	default:
		return Result{}, ErrCursorConflict
	}
	return result, nil
}
