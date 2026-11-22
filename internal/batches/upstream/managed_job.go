package upstream

import (
	"encoding/json"
	"errors"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

type managedJob struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	Endpoint      string          `json:"endpoint"`
	InputFileID   string          `json:"input_file_id"`
	InputFiles    []string        `json:"input_files"`
	OutputFileID  string          `json:"output_file_id"`
	ErrorFileID   string          `json:"error_file_id"`
	OutputFile    string          `json:"output_file"`
	ErrorFile     string          `json:"error_file"`
	CreatedAt     json.RawMessage `json:"created_at"`
	CompletedAt   json.RawMessage `json:"completed_at"`
	TotalRequests *Count          `json:"total_requests"`
	Succeeded     *Count          `json:"succeeded_requests"`
	Failed        *Count          `json:"failed_requests"`
	Metadata      json.RawMessage `json:"metadata"`
	state         string
}

func decodeManagedJob(adapter connectors.BatchAdapter, body []byte) (managedJob, error) {
	var envelope struct {
		Job json.RawMessage `json:"job"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return managedJob{}, errors.New("provider batch object is malformed")
	}
	raw := body
	if adapter == connectors.BatchTogether && !isNull(envelope.Job) {
		raw = envelope.Job
	}
	var job managedJob
	if json.Unmarshal(raw, &job) != nil || job.ID == "" {
		return managedJob{}, errors.New("provider batch object is malformed")
	}
	state, known := managedStatuses[adapter][job.Status]
	if !known {
		return managedJob{}, errors.New("provider batch status is unknown")
	}
	job.state = state
	job.InputFileID = firstNonEmpty(job.InputFileID, first(job.InputFiles))
	job.OutputFileID = firstNonEmpty(job.OutputFileID, job.OutputFile)
	job.ErrorFileID = firstNonEmpty(job.ErrorFileID, job.ErrorFile)
	if len(job.ID) > 512 || job.Endpoint == "" || job.InputFileID == "" || len(job.InputFileID) > 512 ||
		len(job.OutputFileID) > 512 || len(job.ErrorFileID) > 512 {
		return managedJob{}, errors.New("provider batch object is malformed")
	}
	return job, nil
}

func decodeManagedSnapshot(adapter connectors.BatchAdapter, body []byte) (Snapshot, error) {
	job, err := decodeManagedJob(adapter, body)
	if err != nil {
		return Snapshot{}, err
	}
	createdAt, err := optionalTime(job.CreatedAt)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		ID: job.ID, Status: job.state, Endpoint: job.Endpoint, InputFileID: job.InputFileID,
		OutputFileID: job.OutputFileID, ErrorFileID: job.ErrorFileID, Expected: job.TotalRequests.Int64(),
		CreatedAt: createdAt, Body: append([]byte(nil), body...), Terminal: wire.BatchStageOf(job.state) == wire.BatchTerminal,
	}, nil
}

func ProjectManagedBatch(adapter connectors.BatchAdapter, body []byte, window string) ([]byte, string, error) {
	job, err := decodeManagedJob(adapter, body)
	if err != nil {
		return nil, "", err
	}
	projected := map[string]any{
		"id": job.ID, "object": "batch", "endpoint": job.Endpoint,
		"input_file_id": job.InputFileID, "completion_window": window, "status": job.state,
	}
	if len(job.Metadata) > 0 {
		projected["metadata"] = job.Metadata
	}
	if job.OutputFileID != "" {
		projected["output_file_id"] = job.OutputFileID
	}
	if job.ErrorFileID != "" {
		projected["error_file_id"] = job.ErrorFileID
	}
	for key, raw := range map[string]json.RawMessage{"created_at": job.CreatedAt, "completed_at": job.CompletedAt} {
		if seconds, ok := UnixSeconds(raw); ok {
			projected[key] = seconds
		} else if len(raw) > 0 && isNull(raw) {
			projected[key] = nil
		}
	}
	if job.TotalRequests != nil && job.Succeeded != nil && job.Failed != nil {
		projected["request_counts"] = map[string]Count{"total": *job.TotalRequests, "completed": *job.Succeeded, "failed": *job.Failed}
	}
	encoded, err := json.Marshal(projected)
	return encoded, job.ID, err
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
