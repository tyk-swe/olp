package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
)

type geminiReader struct {
	transport transport
}

func (r geminiReader) Poll(ctx context.Context, settlement batches.Settlement, target Target) (Snapshot, error) {
	if settlement.UpstreamID == "" {
		return Snapshot{}, errors.New("Gemini batch upstream identity is unavailable")
	}
	path := "batches/" + url.PathEscape(settlement.UpstreamID)
	summary, err := r.transport.getResource(ctx, target, path, url.Values{"fields": {geminiSummaryFields}}, objectLimit)
	if errors.Is(err, errResponseTooLarge) {
		return r.pollTerminal(ctx, target, path, settlement.UpstreamID)
	}
	if err != nil {
		return Snapshot{}, err
	}
	snapshot, err := decodeGeminiSnapshot(summary)
	if err != nil {
		return Snapshot{}, err
	}
	if snapshot.ID != settlement.UpstreamID {
		return Snapshot{}, ErrProviderConflict
	}
	if !snapshot.Terminal {
		return snapshot, nil
	}
	return r.pollTerminal(ctx, target, path, settlement.UpstreamID)
}

func (r geminiReader) pollTerminal(ctx context.Context, target Target, path, id string) (Snapshot, error) {
	body, err := r.transport.getResource(ctx, target, path, nil, batches.MaxBatchBytes)
	if err != nil {
		return Snapshot{}, tooLarge(err)
	}
	snapshot, err := decodeGeminiSnapshot(body)
	if err != nil || !snapshot.Terminal || snapshot.ID != id {
		return Snapshot{}, ErrProviderConflict
	}
	return snapshot, nil
}

func geminiSubmissionFile(submission batches.Submission) (string, error) {
	switch submission := submission.(type) {
	case batches.GeminiFileSubmission:
		return submission.GeminiFile, nil
	case batches.GeminiManagedSubmission:
		return submission.GeminiFile, nil
	case batches.GeminiInlineSubmission:
		return "", ErrNoEvidence
	default:
		return "", ErrProviderConflict
	}
}

func (r geminiReader) Reconcile(ctx context.Context, settlement batches.Settlement, target Target, known KnownBatch) ([]Snapshot, error) {
	input, err := geminiSubmissionFile(settlement.Submission)
	if err != nil {
		return nil, err
	}
	if input == "" {
		return nil, ErrNoEvidence
	}
	earliest := settlement.CreatedAt.Add(-reconcileSkew)
	matches, err := findSubmission(ctx, known, func(token string, _ int) ([]Snapshot, string, error) {
		return r.listPage(ctx, target, token)
	}, func(snapshot Snapshot) bool {
		return snapshot.InputFileID == input && !snapshot.CreatedAt.IsZero() && !snapshot.CreatedAt.Before(earliest)
	})
	if err != nil || len(matches) != 1 {
		return matches, err
	}
	adopted := settlement
	adopted.UpstreamID = matches[0].ID
	snapshot, err := r.Poll(ctx, adopted, target)
	if err != nil {
		return nil, err
	}
	if snapshot.InputFileID != input {
		return nil, ErrProviderConflict
	}
	return []Snapshot{snapshot}, nil
}

func (r geminiReader) listPage(ctx context.Context, target Target, token string) ([]Snapshot, string, error) {
	query := url.Values{"pageSize": {"100"}, "fields": {geminiListingFields}}
	if token != "" {
		query.Set("pageToken", token)
	}
	body, err := r.transport.getResource(ctx, target, "batches", query, listingLimit)
	if err != nil {
		return nil, "", err
	}
	var listing struct {
		Operations    []json.RawMessage `json:"operations"`
		BatchJobs     []json.RawMessage `json:"batchJobs"`
		NextPageToken string            `json:"nextPageToken"`
	}
	if json.Unmarshal(body, &listing) != nil {
		return nil, "", ErrProviderConflict
	}
	operations := listing.Operations
	if len(operations) == 0 {
		operations = listing.BatchJobs
	}
	snapshots := make([]Snapshot, 0, len(operations))
	for _, raw := range operations {
		snapshot, err := decodeGeminiSnapshot(raw)
		if err != nil {
			return nil, "", err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, listing.NextPageToken, nil
}

func (r geminiReader) Adopt(settlement batches.Settlement, snapshot Snapshot) (Adoption, error) {
	switch submission := settlement.Submission.(type) {
	case batches.GeminiFileSubmission:
		metadata, err := GeminiMetadata(snapshot.Body, settlement.Model(), submission.LocalInputFileID, submission.GeminiFile)
		return Adoption{State: snapshot.Status, Metadata: metadata}, err
	case batches.GeminiManagedSubmission:
		return geminiManagedAdoption(submission, snapshot)
	default:
		return Adoption{}, ErrProviderConflict
	}
}

func geminiManagedAdoption(submission batches.GeminiManagedSubmission, snapshot Snapshot) (Adoption, error) {
	var saved map[string]json.RawMessage
	if json.Unmarshal(submission.Metadata, &saved) != nil || saved == nil {
		return Adoption{}, ErrProviderConflict
	}
	status, ok := GeminiStatus(snapshot.Status)
	if !ok {
		return Adoption{}, ErrProviderConflict
	}
	lifecycle, err := geminiLifecycle(snapshot.Body)
	if err != nil {
		return Adoption{}, err
	}
	metadata, err := mergeFields(saved, map[string]any{"provider_batch": lifecycle, "status": status})
	return Adoption{State: status, Metadata: metadata}, err
}

func (r geminiReader) ReadResults(ctx context.Context, target Target, snapshot Snapshot, cursor batches.Cursor, commit PageCommit) (bool, error) {
	if cursor.Phase == batches.PhaseDone {
		return true, nil
	}
	if !snapshot.Terminal || snapshot.ID == "" {
		return false, nil
	}
	batch, err := DecodeGeminiBatch(snapshot.Body)
	if err != nil {
		return false, err
	}
	parse := func(line []byte, _ int64) (batches.Result, error) { return parseGeminiLine(line, target.Model) }
	if len(batch.inline.items) > 0 {
		lines, err := geminiInlineLines(batch.inline.items)
		if err != nil {
			return false, err
		}
		next, _, _, err := readPages(bytes.NewReader(lines), cursor, "inline:"+snapshot.ID, batches.PhaseOutput, parse, commit, batches.MaxBatchBytes)
		if err != nil {
			return false, err
		}
		next.Phase = batches.PhaseDone
		return true, commit(nil, next)
	}
	if snapshot.OutputFileID == "" || batch.inline.present {
		cursor.Phase = batches.PhaseDone
		return true, commit(nil, cursor)
	}
	if cursor.File != "" && cursor.File != snapshot.OutputFileID {
		return false, batches.ErrCursorConflict
	}
	endpoint, err := geminiDownloadURL(target, snapshot.OutputFileID)
	if err != nil {
		return false, err
	}
	next, err := stream(ctx, r.transport, target, endpoint, r.transport.provider(target, target.Config, nil), batches.MaxBatchBytes,
		func(body io.Reader, remaining int64) (batches.Cursor, int64, error) {
			next, consumed, _, err := readPages(body, cursor, snapshot.OutputFileID, batches.PhaseOutput, parse, commit, remaining)
			return next, consumed, err
		})
	if err != nil {
		return false, err
	}
	next.Phase = batches.PhaseDone
	return true, commit(nil, next)
}

type geminiInlineLine struct {
	Error    json.RawMessage `json:"error,omitempty"`
	Key      string          `json:"key"`
	Response json.RawMessage `json:"response,omitempty"`
}

func geminiInlineLines(items []json.RawMessage) ([]byte, error) {
	lines := make([][]byte, 0, len(items))
	for _, item := range items {
		var result struct {
			Metadata struct {
				Key string `json:"key"`
			} `json:"metadata"`
			Response json.RawMessage `json:"response"`
			Error    json.RawMessage `json:"error"`
		}
		if json.Unmarshal(item, &result) != nil || result.Metadata.Key == "" {
			return nil, batches.ErrCursorConflict
		}
		line := geminiInlineLine{Key: result.Metadata.Key, Response: nonNull(result.Response), Error: nonNull(result.Error)}
		if line.Response == nil && line.Error == nil {
			return nil, batches.ErrCursorConflict
		}
		encoded, err := json.Marshal(line)
		if err != nil {
			return nil, batches.ErrCursorConflict
		}
		lines = append(lines, append(encoded, '\n'))
	}
	return bytes.Join(lines, nil), nil
}

func nonNull(raw json.RawMessage) json.RawMessage {
	if isNull(raw) {
		return nil
	}
	return raw
}

func parseGeminiLine(line []byte, model string) (batches.Result, error) {
	canonical, err := canonicalJSON(line)
	if err != nil {
		return batches.Result{}, batches.ErrCursorConflict
	}
	var object struct {
		Key      string          `json:"key"`
		Response json.RawMessage `json:"response"`
		Error    json.RawMessage `json:"error"`
	}
	if json.Unmarshal(canonical, &object) != nil || object.Key == "" {
		return batches.Result{}, batches.ErrCursorConflict
	}
	failed, answered := !isNull(object.Error), !isNull(object.Response)
	switch {
	case failed && answered, !failed && !answered:
		return batches.Result{}, batches.ErrCursorConflict
	case failed:
		return batches.Result{
			CustomID: object.Key, Kind: batches.ResultErrored, ErrorClass: "gemini_batch_errored",
			Evidence: canonical, Usage: usage.AttemptUsage{Evidence: usage.EvidenceUnobservedComplete},
		}, nil
	}
	completion, err := protocols.Decode(wire.Gemini, wire.Gemini, object.Response, model, "")
	if err != nil || completion == nil {
		return batches.Result{}, batches.ErrCursorConflict
	}
	return batches.Result{
		CustomID: object.Key, Kind: batches.ResultSucceeded, Evidence: canonical,
		Usage: usage.ObservedAttemptUsage(completion.Usage, completion.Usage != nil, string(wire.LineGeneration)),
	}, nil
}

func GeminiFileID(name string) (string, error) {
	prefix, id, ok := strings.Cut(name, "/")
	if !ok || prefix != "files" || id == "" || strings.Contains(id, "/") {
		return "", ErrProviderConflict
	}
	return id, nil
}

func geminiDownloadURL(target Target, fileName string) (string, error) {
	fileID, err := GeminiFileID(fileName)
	if err != nil {
		return "", err
	}
	return GeminiDownloadURL(target.Config, target.Model, fileID)
}

func GeminiDownloadURL(config connectors.Config, model, fileID string) (string, error) {
	generation, err := config.URL(wire.Gemini, model, false)
	if err != nil {
		return "", ErrProviderConflict
	}
	parsed, err := url.Parse(generation)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", ErrProviderConflict
	}
	return parsed.Scheme + "://" + parsed.Host + "/download/v1beta/files/" + url.PathEscape(fileID) + ":download?alt=media", nil
}
