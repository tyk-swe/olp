package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
)

const (
	geminiSummaryFields = "name,done,error,metadata(state,createTime,inputConfig(fileName),batchStats)"
	geminiListingFields = "operations(name,done,error(code),metadata(state,createTime,inputConfig(fileName),batchStats)),nextPageToken"
)

var geminiStatuses = map[string]string{
	"BATCH_STATE_PENDING": "validating", "JOB_STATE_PENDING": "validating", "JOB_STATE_QUEUED": "validating",
	"BATCH_STATE_RUNNING": "in_progress", "JOB_STATE_RUNNING": "in_progress",
	"BATCH_STATE_CANCELLING": "cancelling", "JOB_STATE_CANCELLING": "cancelling",
	"BATCH_STATE_SUCCEEDED": "completed", "JOB_STATE_SUCCEEDED": "completed",
	"BATCH_STATE_FAILED": "failed", "JOB_STATE_FAILED": "failed",
	"BATCH_STATE_CANCELLED": "cancelled", "JOB_STATE_CANCELLED": "cancelled",
	"BATCH_STATE_EXPIRED": "expired", "JOB_STATE_EXPIRED": "expired",
}

var (
	geminiLifecycleScalars = []string{"name", "model", "state", "displayName", "createTime", "startTime", "endTime", "updateTime", "done",
		"requestCount", "successfulRequestCount", "failedRequestCount", "pendingRequestCount", "fileName", "file_name", "responsesFile", "code"}
	geminiLifecycleObjects = append([]string{"metadata", "batchStats", "inputConfig", "input_config", "error"}, GeminiOutputFields()...)
)

func GeminiOutputFields() []string {
	return []string{"output", "response"}
}

func GeminiStatus(state string) (string, bool) {
	status, ok := geminiStatuses[state]
	return status, ok
}

type geminiInput struct {
	FileName      string `json:"fileName"`
	FileNameSnake string `json:"file_name"`
}

type inlineResponses struct {
	items   []json.RawMessage
	present bool
}

func (r *inlineResponses) UnmarshalJSON(raw []byte) error {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		*r = inlineResponses{items: items, present: items != nil}
		return nil
	}
	var nested struct {
		Items []json.RawMessage `json:"inlinedResponses"`
	}
	if err := json.Unmarshal(raw, &nested); err != nil {
		return err
	}
	*r = inlineResponses{items: nested.Items, present: nested.Items != nil}
	return nil
}

type geminiOutput struct {
	ResponsesFile    string          `json:"responsesFile"`
	InlinedResponses inlineResponses `json:"inlinedResponses"`
}

type geminiFields struct {
	State       string       `json:"state"`
	CreateTime  string       `json:"createTime"`
	InputConfig *geminiInput `json:"inputConfig"`
	InputSnake  *geminiInput `json:"input_config"`
	BatchStats  *struct {
		RequestCount *Count `json:"requestCount"`
	} `json:"batchStats"`
	Output   *geminiOutput `json:"output"`
	Response *geminiOutput `json:"response"`
}

func (f geminiFields) inputFile() string {
	names := []string{}
	for _, input := range []*geminiInput{f.InputConfig, f.InputSnake} {
		if input != nil {
			names = append(names, input.FileName, input.FileNameSnake)
		}
	}
	return firstNonEmpty(names...)
}

func (f geminiFields) outputs() []geminiOutput {
	return slices.Collect(func(yield func(geminiOutput) bool) {
		for _, output := range []*geminiOutput{f.Output, f.Response} {
			if output != nil && !yield(*output) {
				return
			}
		}
	})
}

type GeminiBatch struct {
	ID         string
	State      string
	InputFile  string
	OutputFile string
	CreatedAt  time.Time
	Expected   *int64
	inline     inlineResponses
}

func DecodeGeminiBatch(body []byte) (GeminiBatch, error) {
	var object struct {
		Name     string        `json:"name"`
		Metadata *geminiFields `json:"metadata"`
		geminiFields
	}
	if json.Unmarshal(body, &object) != nil {
		return GeminiBatch{}, ErrProviderConflict
	}
	id, ok := strings.CutPrefix(object.Name, "batches/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return GeminiBatch{}, ErrProviderConflict
	}
	layers := []geminiFields{object.geminiFields}
	if object.Metadata != nil {
		layers = []geminiFields{*object.Metadata, object.geminiFields}
	}
	return GeminiBatch{
		ID:         id,
		State:      firstLayer(layers, func(f geminiFields) string { return f.State }),
		InputFile:  firstLayer(layers, geminiFields.inputFile),
		OutputFile: firstLayer(layers, geminiOutputFile),
		CreatedAt:  firstLayer(layers, geminiCreatedAt),
		Expected:   firstLayer(layers, geminiRequestCount),
		inline:     geminiInline(layers),
	}, nil
}

func firstLayer[T comparable](layers []geminiFields, get func(geminiFields) T) T {
	var zero T
	for _, layer := range layers {
		if value := get(layer); value != zero {
			return value
		}
	}
	return zero
}

func geminiOutputFile(f geminiFields) string {
	files := []string{}
	for _, output := range f.outputs() {
		files = append(files, output.ResponsesFile)
	}
	return firstNonEmpty(files...)
}

func geminiCreatedAt(f geminiFields) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, f.CreateTime)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func geminiRequestCount(f geminiFields) *int64 {
	if f.BatchStats == nil {
		return nil
	}
	return f.BatchStats.RequestCount.Int64()
}

func geminiInline(layers []geminiFields) inlineResponses {
	for _, layer := range layers {
		for _, output := range layer.outputs() {
			if output.InlinedResponses.present {
				return output.InlinedResponses
			}
		}
	}
	return inlineResponses{}
}

func (b GeminiBatch) snapshot(body []byte) (Snapshot, error) {
	status, known := GeminiStatus(b.State)
	if !known {
		return Snapshot{}, ErrProviderConflict
	}
	output := b.OutputFile
	if output == "" && b.inline.present {
		output = "inline:" + b.ID
	}
	return Snapshot{
		ID: b.ID, Status: b.State, InputFileID: b.InputFile, OutputFileID: output, Expected: b.Expected,
		CreatedAt: b.CreatedAt, Body: bytes.Clone(body), Terminal: wire.BatchStageOf(status) == wire.BatchTerminal,
	}, nil
}

func decodeGeminiSnapshot(body []byte) (GeminiBatch, Snapshot, error) {
	batch, err := DecodeGeminiBatch(body)
	if err != nil {
		return GeminiBatch{}, Snapshot{}, err
	}
	snapshot, err := batch.snapshot(body)
	return batch, snapshot, err
}

func GeminiMetadata(body []byte, model, localInput, upstreamInput string) (json.RawMessage, error) {
	fields, err := geminiLifecycle(body)
	if err != nil {
		return nil, err
	}
	extras := map[string]any{"upstream_model": model}
	if localInput != "" {
		extras["local_input_file_name"], extras["upstream_input_file_name"] = localInput, upstreamInput
	}
	return mergeFields(fields, extras)
}

func geminiLifecycle(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var source map[string]json.RawMessage
	if json.Unmarshal(raw, &source) != nil {
		return nil, errors.New("Gemini batch lifecycle is malformed")
	}
	fields := make(map[string]json.RawMessage, len(source))
	for key, value := range source {
		if slices.Contains(geminiLifecycleScalars, key) {
			fields[key] = value
			continue
		}
		if !slices.Contains(geminiLifecycleObjects, key) {
			continue
		}
		nested, err := geminiLifecycle(value)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(nested)
		if err != nil {
			return nil, err
		}
		fields[key] = encoded
	}
	return fields, nil
}

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
	_, snapshot, err := decodeGeminiSnapshot(summary)
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
	_, snapshot, err := decodeGeminiSnapshot(body)
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
		_, snapshot, err := decodeGeminiSnapshot(raw)
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
