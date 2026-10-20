package batches

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

type managedReader struct {
	adapter string
	auth    *connectors.Auth
	egress  *egress.Policy
}

func (r *managedReader) Poll(ctx context.Context, settlement Settlement, target *PinnedTarget) (BatchSnapshot, error) {
	if settlement.UpstreamID == nil {
		return BatchSnapshot{}, errors.New("batch upstream identity is unavailable")
	}
	path := "batches/" + url.PathEscape(*settlement.UpstreamID)
	if r.adapter == "mistral" {
		path = "batch/jobs/" + url.PathEscape(*settlement.UpstreamID)
	}
	reader := OpenAIReader{Auth: r.auth, Egress: r.egress}
	body, err := reader.getJSON(ctx, target, path, nil, 1<<20)
	if err != nil {
		return BatchSnapshot{}, err
	}
	snapshot, err := parseManagedSnapshot(body, r.adapter)
	if err == nil {
		if settlement.UpstreamID == nil || snapshot.ID != *settlement.UpstreamID {
			return BatchSnapshot{}, ErrProviderConflict
		}
		inputFileID, _, endpoint, parseErr := submissionFiles(settlement.Cursor)
		if parseErr != nil || endpoint != snapshot.Endpoint || inputFileID != "" && snapshot.InputFileID != inputFileID ||
			lineOperation(snapshot.Endpoint) != settlement.LineOperation {
			return BatchSnapshot{}, ErrProviderConflict
		}
		snapshot.Expected = settlement.ExpectedResults
	}
	return snapshot, err
}

func (r *managedReader) Reconcile(ctx context.Context, settlement Settlement, target *PinnedTarget, inputFileID, endpoint string) (Reconciliation, error) {
	listReader := OpenAIReader{Auth: r.auth, Egress: r.egress}
	var matches []BatchSnapshot
	for page := 0; page < 10; page++ {
		path := "batches"
		query := url.Values{}
		if r.adapter == "mistral" {
			path = "batch/jobs"
			query.Set("page", strconv.Itoa(page))
			query.Set("page_size", "100")
		}
		body, err := listReader.getJSON(ctx, target, path, query, 4<<20)
		if err != nil {
			return Reconciliation{}, err
		}
		items, hasMore, err := managedBatchList(body, r.adapter, page)
		if err != nil {
			return Reconciliation{}, err
		}
		for _, item := range items {
			if item.InputFileID == inputFileID && !item.CreatedAt.Before(settlement.CreatedAt.Add(-5*time.Minute)) {
				matches = append(matches, item)
				if len(matches) > 1 {
					return Reconciliation{Matches: matches}, nil
				}
			}
		}
		if !hasMore {
			if len(matches) == 1 && matches[0].Endpoint != endpoint {
				return Reconciliation{}, ErrProviderConflict
			}
			return Reconciliation{Matches: matches}, nil
		}
	}
	return Reconciliation{}, errors.New("provider batch list exceeded the reconciliation page limit")
}

func (r *managedReader) ReadResults(ctx context.Context, target *PinnedTarget, snapshot BatchSnapshot, cursor Cursor, commit ResultPageCommit) (Cursor, bool, error) {
	reader := OpenAIReader{Auth: r.auth, Egress: r.egress}
	return reader.ReadResults(ctx, target, snapshot, cursor, commit)
}

func parseManagedSnapshot(body []byte, adapter string) (BatchSnapshot, error) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil || raw == nil {
		return BatchSnapshot{}, errors.New("provider batch object is malformed")
	}
	var job map[string]json.RawMessage
	if adapter == "together" {
		if wrapped, ok := raw["job"]; ok {
			if json.Unmarshal(wrapped, &job) != nil || job == nil {
				return BatchSnapshot{}, errors.New("Together batch response has no job")
			}
		} else {
			job = raw
		}
	} else {
		job = raw
	}
	status := jsonText(job["status"])
	mapped := managedBatchStatuses[adapter][status]
	if mapped == "" {
		return BatchSnapshot{}, errors.New("provider batch status is unknown")
	}
	id := jsonText(job["id"])
	endpoint := jsonText(job["endpoint"])
	input := jsonText(job["input_file_id"])
	if adapter == "mistral" && input == "" {
		var inputs []string
		_ = json.Unmarshal(job["input_files"], &inputs)
		if len(inputs) > 0 {
			input = inputs[0]
		}
	}
	output := jsonText(job["output_file_id"])
	if output == "" {
		output = jsonText(job["output_file"])
	}
	errorID := jsonText(job["error_file_id"])
	if errorID == "" {
		errorID = jsonText(job["error_file"])
	}
	if id == "" || endpoint == "" || input == "" || len(id) > 512 || len(input) > 512 || len(output) > 512 || len(errorID) > 512 {
		return BatchSnapshot{}, errors.New("provider batch object is malformed")
	}
	expected := (*int64)(nil)
	if adapter == "mistral" {
		if n, ok := jsonInt64(job["total_requests"]); ok {
			expected = &n
		}
	}
	created, _ := managedUnix(job["created_at"])
	normalized := map[string]any{"id": id, "status": mapped, "endpoint": endpoint, "input_file_id": input,
		"output_file_id": output, "error_file_id": errorID, "created_at": created}
	if metadata, ok := job["metadata"]; ok {
		normalized["metadata"] = metadata
	}
	if expected != nil {
		normalized["request_counts"] = map[string]any{"total": *expected}
	}
	bodyNormalized, _ := json.Marshal(normalized)
	return BatchSnapshot{
		ID: id, Status: mapped, Endpoint: endpoint, InputFileID: input, OutputFileID: output,
		ErrorFileID: errorID, Expected: expected, CreatedAt: time.Unix(created, 0),
		Body: bodyNormalized, Terminal: terminalStatus(mapped),
	}, nil
}

func managedUnix(raw json.RawMessage) (int64, bool) {
	if value, ok := jsonInt64(raw); ok {
		return value, true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed.Unix(), true
		}
	}
	return 0, false
}

func managedBatchList(body []byte, adapter string, page int) ([]BatchSnapshot, bool, error) {
	var items []json.RawMessage
	hasMore := false
	if adapter == "together" {
		if json.Unmarshal(body, &items) != nil {
			return nil, false, errors.New("Together batch list is malformed")
		}
	} else {
		var listing struct {
			Data    []json.RawMessage `json:"data"`
			HasMore bool              `json:"has_more"`
			Total   int               `json:"total"`
		}
		if json.Unmarshal(body, &listing) != nil {
			return nil, false, errors.New("Mistral batch list is malformed")
		}
		items, hasMore = listing.Data, listing.HasMore
		if adapter == "mistral" && len(items) == 100 && listing.Total > (page+1)*100 {
			hasMore = true
		}
	}
	result := make([]BatchSnapshot, 0, len(items))
	for _, item := range items {
		snapshot, err := parseManagedSnapshot(item, adapter)
		if err != nil {
			return nil, false, err
		}
		result = append(result, snapshot)
	}
	return result, hasMore, nil
}

func jsonText(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func jsonInt64(raw json.RawMessage) (int64, bool) {
	var value int64
	if json.Unmarshal(raw, &value) == nil && value >= 0 {
		return value, true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		value, err := strconv.ParseInt(text, 10, 64)
		return value, err == nil && value >= 0
	}
	return 0, false
}

var managedBatchStatuses = map[string]map[string]string{
	"together": {"VALIDATING": "validating", "IN_PROGRESS": "in_progress", "COMPLETED": "completed", "FAILED": "failed", "EXPIRED": "expired", "CANCELLING": "cancelling", "CANCELLED": "cancelled"},
	"mistral":  {"QUEUED": "validating", "RUNNING": "in_progress", "SUCCESS": "completed", "FAILED": "failed", "TIMEOUT_EXCEEDED": "expired", "CANCELLATION_REQUESTED": "cancelling", "CANCELLED": "cancelled"},
}
