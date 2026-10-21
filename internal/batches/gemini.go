package batches

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
)

const geminiListPages = 10

type GeminiReader struct {
	Auth   *connectors.Auth
	Egress *egress.Policy
}

// GeminiMetadata retains lifecycle and file identity fields, never inline
// requests or generated responses. Result bodies are handled only in memory.
func GeminiMetadata(body []byte, model, localInput, upstreamInput string) json.RawMessage {
	fields := geminiLifecycleFields(body)
	fields["upstream_model"], _ = json.Marshal(model)
	if localInput != "" {
		fields["local_input_file_name"], _ = json.Marshal(localInput)
		fields["upstream_input_file_name"], _ = json.Marshal(upstreamInput)
	}
	encoded, _ := json.Marshal(fields)
	return encoded
}

func geminiLifecycleFields(body []byte) map[string]json.RawMessage {
	var source map[string]json.RawMessage
	_ = json.Unmarshal(body, &source)
	fields := map[string]json.RawMessage{}
	for _, key := range []string{"name", "model", "state", "displayName", "createTime", "startTime", "endTime", "updateTime", "done",
		"requestCount", "successfulRequestCount", "failedRequestCount", "pendingRequestCount", "fileName", "file_name", "responsesFile", "code"} {
		if raw, ok := source[key]; ok {
			fields[key] = raw
		}
	}
	for _, key := range []string{"metadata", "batchStats", "inputConfig", "input_config", "output", "response", "error"} {
		if raw, ok := source[key]; ok {
			fields[key], _ = json.Marshal(geminiLifecycleFields(raw))
		}
	}
	return fields
}

func geminiManagedAdoptMetadata(cursor []byte, snapshot BatchSnapshot) (json.RawMessage, string, error) {
	var saved struct {
		Submission struct {
			Metadata map[string]json.RawMessage `json:"gemini_metadata"`
		} `json:"submission"`
	}
	if json.Unmarshal(cursor, &saved) != nil || saved.Submission.Metadata == nil {
		return nil, "", ErrProviderConflict
	}
	state := map[string]string{
		"PENDING": "validating", "QUEUED": "validating", "RUNNING": "in_progress",
		"SUCCEEDED": "completed", "FAILED": "failed", "CANCELLED": "cancelled",
		"EXPIRED": "expired", "CANCELLING": "cancelling",
	}[strings.TrimPrefix(strings.TrimPrefix(batchGeminiState(snapshot.Body), "BATCH_STATE_"), "JOB_STATE_")]
	if state == "" {
		return nil, "", ErrProviderConflict
	}
	fields := saved.Submission.Metadata
	fields["provider_batch"], _ = json.Marshal(geminiLifecycleFields(snapshot.Body))
	fields["status"], _ = json.Marshal(state)
	encoded, err := json.Marshal(fields)
	return encoded, state, err
}

func (r *GeminiReader) Poll(ctx context.Context, settlement Settlement, target *PinnedTarget) (BatchSnapshot, error) {
	if settlement.UpstreamID == nil || *settlement.UpstreamID == "" {
		return BatchSnapshot{}, errors.New("Gemini batch upstream identity is unavailable")
	}
	path := "batches/" + url.PathEscape(*settlement.UpstreamID)
	reader := &OpenAIReader{Auth: r.Auth, Egress: r.Egress}
	fields := url.Values{"fields": {"name,done,error,metadata(state,createTime,inputConfig(fileName),batchStats)"}}
	body, err := reader.getJSON(ctx, target, path, fields, 1<<20)
	if err != nil {
		if !strings.Contains(err.Error(), "exceeds the limit") {
			return BatchSnapshot{}, err
		}
		body, err = reader.getJSON(ctx, target, path, nil, MaxBatchBytes)
		if err != nil {
			if strings.Contains(err.Error(), "exceeds the limit") {
				return BatchSnapshot{}, ErrBatchTooLarge
			}
			return BatchSnapshot{}, err
		}
		snapshot, parseErr := parseGeminiBatchSnapshot(body)
		if parseErr != nil || !snapshot.Terminal {
			return BatchSnapshot{}, ErrProviderConflict
		}
		if snapshot.ID != *settlement.UpstreamID {
			return BatchSnapshot{}, ErrProviderConflict
		}
		snapshot.Body = body
		return snapshot, nil
	}
	summary, err := parseGeminiBatchSnapshot(body)
	if err != nil {
		return BatchSnapshot{}, err
	}
	if summary.ID != *settlement.UpstreamID {
		return BatchSnapshot{}, ErrProviderConflict
	}
	if !summary.Terminal {
		summary.Body = body
		return summary, nil
	}
	terminal, err := reader.getJSON(ctx, target, path, nil, MaxBatchBytes)
	if err != nil {
		if strings.Contains(err.Error(), "exceeds the limit") {
			return BatchSnapshot{}, ErrBatchTooLarge
		}
		return BatchSnapshot{}, err
	}
	snapshot, err := parseGeminiBatchSnapshot(terminal)
	if err != nil || !snapshot.Terminal || snapshot.ID != *settlement.UpstreamID {
		return BatchSnapshot{}, ErrProviderConflict
	}
	snapshot.Body = terminal
	return snapshot, nil
}

func parseGeminiBatchSnapshot(body []byte) (BatchSnapshot, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return BatchSnapshot{}, ErrProviderConflict
	}
	var name string
	if json.Unmarshal(object["name"], &name) != nil {
		return BatchSnapshot{}, ErrProviderConflict
	}
	id, ok := strings.CutPrefix(name, "batches/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return BatchSnapshot{}, ErrProviderConflict
	}
	state := batchGeminiState(body)
	terminal, known := geminiBatchTerminal(state)
	if !known {
		return BatchSnapshot{}, ErrProviderConflict
	}
	snapshot := BatchSnapshot{
		ID: id, Status: state, InputFileID: batchGeminiInputName(body),
		CreatedAt: geminiBatchCreatedAt(body), Body: bytes.Clone(body), Terminal: terminal,
	}
	if count, present, err := geminiRequestCount(body); err != nil {
		return BatchSnapshot{}, err
	} else if present {
		snapshot.Expected = &count
	}
	if file := batchGeminiOutputFile(body); file != "" {
		snapshot.OutputFileID = file
	} else if batchGeminiInlineResponses(body) != nil {
		snapshot.OutputFileID = "inline:" + id
	}
	return snapshot, nil
}

func geminiBatchTerminal(state string) (bool, bool) {
	switch state {
	case "BATCH_STATE_SUCCEEDED", "JOB_STATE_SUCCEEDED",
		"BATCH_STATE_FAILED", "JOB_STATE_FAILED",
		"BATCH_STATE_CANCELLED", "JOB_STATE_CANCELLED",
		"BATCH_STATE_EXPIRED", "JOB_STATE_EXPIRED":
		return true, true
	case "BATCH_STATE_PENDING", "JOB_STATE_PENDING", "JOB_STATE_QUEUED",
		"BATCH_STATE_RUNNING", "JOB_STATE_RUNNING", "BATCH_STATE_CANCELLING", "JOB_STATE_CANCELLING":
		return false, true
	default:
		return false, false
	}
}

func geminiBatchCreatedAt(body []byte) time.Time {
	var object map[string]json.RawMessage
	_ = json.Unmarshal(body, &object)
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(object["metadata"], &metadata)
	for _, raw := range []json.RawMessage{metadata["createTime"], object["createTime"]} {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
				return parsed
			}
		}
	}
	return time.Time{}
}

func geminiRequestCount(body []byte) (int64, bool, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return 0, false, ErrProviderConflict
	}
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(object["metadata"], &metadata)
	var stats map[string]json.RawMessage
	for _, raw := range []json.RawMessage{metadata["batchStats"], object["batchStats"]} {
		stats = nil
		_ = json.Unmarshal(raw, &stats)
		if stats != nil {
			break
		}
	}
	raw, present := stats["requestCount"]
	if !present {
		return 0, false, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		count, err := strconv.ParseInt(text, 10, 64)
		if err != nil || count < 0 {
			return 0, false, ErrProviderConflict
		}
		return count, true, nil
	}
	var count int64
	if json.Unmarshal(raw, &count) != nil || count < 0 {
		return 0, false, ErrProviderConflict
	}
	return count, true, nil
}

func (r *GeminiReader) ReadResults(ctx context.Context, target *PinnedTarget, snapshot BatchSnapshot, cursor Cursor, commit ResultPageCommit) (Cursor, bool, error) {
	if cursor.Phase == "done" {
		return cursor, true, nil
	}
	if !snapshot.Terminal || snapshot.ID == "" {
		return cursor, false, nil
	}
	if inline := batchGeminiInlineResponses(snapshot.Body); len(inline) > 0 {
		data, err := geminiInlineJSONL(inline)
		if err != nil {
			return cursor, false, err
		}
		file := "inline:" + snapshot.ID
		next, _, _, err := ReadJSONLPages(bytes.NewReader(data), cursor, file, "output",
			func(line []byte) (Result, error) { return parseGeminiResult(line, target.Model) }, commit)
		if err != nil {
			return next, false, err
		}
		next.Phase = "done"
		if err = commit(nil, next); err != nil {
			return next, false, err
		}
		return next, true, nil
	}
	if snapshot.OutputFileID == "" {
		cursor.Phase = "done"
		if err := commit(nil, cursor); err != nil {
			return cursor, false, err
		}
		return cursor, true, nil
	}
	if cursor.File != "" && cursor.File != snapshot.OutputFileID {
		return cursor, false, ErrCursorConflict
	}
	endpoint, err := geminiBatchDownloadURL(target, snapshot.OutputFileID)
	if err != nil {
		return cursor, false, err
	}
	response, err := r.doURL(ctx, target, http.MethodGet, endpoint)
	if err != nil {
		return cursor, false, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return cursor, false, errors.New("Gemini batch output file download failed")
	}
	if response.ContentLength > MaxBatchBytes {
		return cursor, false, ErrBatchTooLarge
	}
	limited := &io.LimitedReader{R: response.Body, N: MaxBatchBytes + 1}
	next, consumed, _, err := ReadJSONLPages(limited, cursor, snapshot.OutputFileID, "output",
		func(line []byte) (Result, error) { return parseGeminiResult(line, target.Model) }, commit)
	if err != nil {
		return next, false, err
	}
	if limited.N == 0 || consumed > MaxBatchBytes {
		return next, false, ErrBatchTooLarge
	}
	next.Phase = "done"
	if err := commit(nil, next); err != nil {
		return next, false, err
	}
	return next, true, nil
}

func geminiInlineJSONL(raw json.RawMessage) ([]byte, error) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, ErrCursorConflict
	}
	var out bytes.Buffer
	for _, item := range items {
		var result map[string]json.RawMessage
		if json.Unmarshal(item, &result) != nil {
			return nil, ErrCursorConflict
		}
		var metadata map[string]json.RawMessage
		if json.Unmarshal(result["metadata"], &metadata) != nil {
			return nil, ErrCursorConflict
		}
		var key string
		if json.Unmarshal(metadata["key"], &key) != nil || key == "" {
			return nil, ErrCursorConflict
		}
		keyRaw, _ := json.Marshal(key)
		line := map[string]json.RawMessage{"key": keyRaw}
		for _, field := range []string{"response", "error"} {
			if rawValue, ok := result[field]; ok && string(rawValue) != "null" {
				line[field] = rawValue
			}
		}
		if len(line) == 1 {
			return nil, ErrCursorConflict
		}
		encoded, err := json.Marshal(line)
		if err != nil {
			return nil, ErrCursorConflict
		}
		out.Write(encoded)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

func parseGeminiResult(line []byte, model string) (Result, error) {
	canonical, err := canonicalJSON(line)
	if err != nil {
		return Result{}, ErrCursorConflict
	}
	var object struct {
		Key      string          `json:"key"`
		Response json.RawMessage `json:"response"`
		Error    json.RawMessage `json:"error"`
	}
	if json.Unmarshal(canonical, &object) != nil || object.Key == "" {
		return Result{}, ErrCursorConflict
	}
	result := Result{CustomID: object.Key, Evidence: canonical, Kind: ResultErrored, ErrorClass: "gemini_batch_errored", Usage: usage.AttemptUsage{Complete: true}}
	if len(object.Error) > 0 && string(object.Error) != "null" {
		if len(object.Response) > 0 && string(object.Response) != "null" {
			return Result{}, ErrCursorConflict
		}
		return result, nil
	}
	if len(object.Response) == 0 || string(object.Response) == "null" {
		return Result{}, ErrCursorConflict
	}
	completion, err := protocols.Decode(wire.Gemini, wire.Gemini, object.Response, model, "")
	if err != nil || completion == nil {
		return Result{}, ErrCursorConflict
	}
	result.Kind = ResultSucceeded
	result.ErrorClass = ""
	result.Usage = usage.ObservedAttemptUsage(completion.Usage, completion.Usage != nil, "generation")
	return result, nil
}

func geminiBatchDownloadURL(target *PinnedTarget, fileName string) (string, error) {
	fileID, err := batchGeminiFileID(fileName)
	if err != nil {
		return "", ErrProviderConflict
	}
	generation, err := target.Config.URL(wire.Gemini, target.Model, false)
	if err != nil {
		return "", ErrProviderConflict
	}
	parsed, err := url.Parse(generation)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", ErrProviderConflict
	}
	return parsed.Scheme + "://" + parsed.Host + "/download/v1beta/files/" + url.PathEscape(fileID) + ":download?alt=media", nil
}

func (r *GeminiReader) doURL(ctx context.Context, target *PinnedTarget, method, endpoint string) (*http.Response, error) {
	if target == nil || target.Client == nil || r.Auth == nil || r.Egress == nil {
		return nil, errors.New("Gemini batch transport is unavailable")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil {
		return nil, ErrProviderConflict
	}
	origin := *parsed
	origin.Path, origin.RawQuery, origin.Fragment = "", "", ""
	if _, err := r.Egress.ValidateEndpoint(origin.String()); err != nil {
		return nil, errors.New("Gemini result file address is outside egress policy")
	}
	request, err := http.NewRequestWithContext(ctx, method, parsed.String(), nil)
	if err != nil {
		return nil, errors.New("build Gemini batch download request")
	}
	request.Header.Set("Accept", "application/json, application/octet-stream")
	request.Header.Set("User-Agent", "olp/batch-settlement")
	if _, err := r.Auth.Apply(ctx, request, target.Config, target.Secret, nil); err != nil {
		return nil, errors.New("Gemini batch credential could not be applied")
	}
	response, err := target.Client.Do(request)
	if err != nil {
		return nil, errors.New("Gemini batch download request failed")
	}
	return response, nil
}

func (r *GeminiReader) Reconcile(ctx context.Context, settlement Settlement, target *PinnedTarget, inputFileName, _ string) (Reconciliation, error) {
	var cursor struct {
		Submission struct {
			File string `json:"gemini_file"`
		} `json:"submission"`
	}
	if json.Unmarshal(settlement.Cursor, &cursor) != nil {
		return Reconciliation{}, ErrProviderConflict
	}
	if cursor.Submission.File == "" || inputFileName == "" || cursor.Submission.File != inputFileName {
		return Reconciliation{}, nil
	}
	reader := &OpenAIReader{Auth: r.Auth, Egress: r.Egress}
	after := ""
	matches := make([]BatchSnapshot, 0, 1)
	for page := 0; page < geminiListPages; page++ {
		query := url.Values{"pageSize": {"100"}}
		if after != "" {
			query.Set("pageToken", after)
		}
		body, err := reader.getJSON(ctx, target, "batches", query, 4<<20)
		if err != nil {
			return Reconciliation{}, err
		}
		var listing struct {
			Operations    []json.RawMessage `json:"operations"`
			BatchJobs     []json.RawMessage `json:"batchJobs"`
			NextPageToken string            `json:"nextPageToken"`
		}
		if json.Unmarshal(body, &listing) != nil {
			return Reconciliation{}, ErrProviderConflict
		}
		operations := listing.Operations
		if len(operations) == 0 {
			operations = listing.BatchJobs
		}
		for _, raw := range operations {
			snapshot, err := parseGeminiBatchSnapshot(raw)
			if err != nil {
				return Reconciliation{}, err
			}
			if snapshot.InputFileID == inputFileName && !snapshot.CreatedAt.IsZero() &&
				!snapshot.CreatedAt.Before(settlement.CreatedAt.Add(-5*time.Minute)) {
				known, err := target.knownBatch(ctx, snapshot.ID)
				if err != nil {
					return Reconciliation{}, err
				}
				if known {
					continue
				}
				matches = append(matches, snapshot)
				if len(matches) > 1 {
					return Reconciliation{Matches: matches}, nil
				}
			}
		}
		if listing.NextPageToken == "" {
			return Reconciliation{Matches: matches}, nil
		}
		if listing.NextPageToken == after {
			return Reconciliation{}, ErrProviderConflict
		}
		after = listing.NextPageToken
	}
	return Reconciliation{}, errors.New("Gemini batch list exceeded the reconciliation page limit")
}

func batchGeminiState(body []byte) string {
	var object struct {
		State    string `json:"state"`
		Metadata struct {
			State string `json:"state"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(body, &object)
	if object.Metadata.State != "" {
		return object.Metadata.State
	}
	return object.State
}

func batchGeminiInputName(body []byte) string {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return ""
	}
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(object["metadata"], &metadata)
	for _, parent := range []map[string]json.RawMessage{metadata, object} {
		for _, key := range []string{"inputConfig", "input_config"} {
			var input map[string]json.RawMessage
			if json.Unmarshal(parent[key], &input) != nil {
				continue
			}
			for _, name := range []string{"fileName", "file_name"} {
				var value string
				if json.Unmarshal(input[name], &value) == nil && value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func batchGeminiOutputFile(body []byte) string {
	var object map[string]json.RawMessage
	_ = json.Unmarshal(body, &object)
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(object["metadata"], &metadata)
	for _, parent := range []map[string]json.RawMessage{metadata, object} {
		for _, field := range []string{"output", "response"} {
			var output map[string]json.RawMessage
			if json.Unmarshal(parent[field], &output) != nil {
				continue
			}
			var file string
			if json.Unmarshal(output["responsesFile"], &file) == nil && file != "" {
				return file
			}
		}
	}
	return ""
}

func batchGeminiInlineResponses(body []byte) json.RawMessage {
	var object map[string]json.RawMessage
	_ = json.Unmarshal(body, &object)
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(object["metadata"], &metadata)
	for _, parent := range []map[string]json.RawMessage{metadata, object} {
		for _, field := range []string{"output", "response"} {
			var output map[string]json.RawMessage
			if json.Unmarshal(parent[field], &output) != nil {
				continue
			}
			raw := output["inlinedResponses"]
			var items []json.RawMessage
			if json.Unmarshal(raw, &items) == nil && items != nil {
				return raw
			}
			var inline map[string]json.RawMessage
			if json.Unmarshal(raw, &inline) == nil && len(inline["inlinedResponses"]) > 0 {
				return inline["inlinedResponses"]
			}
		}
	}
	return nil
}

func batchGeminiFileID(name string) (string, error) {
	prefix, id, ok := strings.Cut(name, "/")
	if !ok || prefix != "files" || id == "" || strings.Contains(id, "/") {
		return "", ErrProviderConflict
	}
	return id, nil
}
