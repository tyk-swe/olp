package upstream

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
)

type CloudStatus string

const (
	CloudQueued             CloudStatus = "queued"
	CloudRunning            CloudStatus = "running"
	CloudSucceeded          CloudStatus = "succeeded"
	CloudPartiallySucceeded CloudStatus = "partially_succeeded"
	CloudFailed             CloudStatus = "failed"
	CloudCancelling         CloudStatus = "cancelling"
	CloudCancelled          CloudStatus = "cancelled"
	CloudExpired            CloudStatus = "expired"
)

var cloudStatuses = map[connectors.BatchAdapter]map[string]CloudStatus{
	connectors.BatchVertex: {
		"JOB_STATE_QUEUED": CloudQueued, "JOB_STATE_PENDING": CloudQueued,
		"JOB_STATE_RUNNING": CloudRunning, "JOB_STATE_PAUSED": CloudRunning, "JOB_STATE_UPDATING": CloudRunning,
		"JOB_STATE_SUCCEEDED": CloudSucceeded, "JOB_STATE_PARTIALLY_SUCCEEDED": CloudPartiallySucceeded,
		"JOB_STATE_FAILED": CloudFailed, "JOB_STATE_CANCELLING": CloudCancelling,
		"JOB_STATE_CANCELLED": CloudCancelled, "JOB_STATE_EXPIRED": CloudExpired,
	},
	connectors.BatchBedrock: {
		"Submitted": CloudQueued, "Validating": CloudQueued, "Scheduled": CloudQueued,
		"InProgress": CloudRunning, "Completed": CloudSucceeded, "PartiallyCompleted": CloudPartiallySucceeded,
		"Failed": CloudFailed, "Stopping": CloudCancelling, "Stopped": CloudCancelled, "Expired": CloudExpired,
	},
}

func (s CloudStatus) Terminal() bool {
	switch s {
	case CloudSucceeded, CloudPartiallySucceeded, CloudFailed, CloudCancelled, CloudExpired:
		return true
	default:
		return false
	}
}

type CloudCounts struct {
	Total      *int64 `json:"total,omitempty"`
	Successful *int64 `json:"successful,omitempty"`
	Failed     *int64 `json:"failed,omitempty"`
	Incomplete *int64 `json:"incomplete,omitempty"`
	Processed  *int64 `json:"processed,omitempty"`
}

type CloudError struct {
	Code    string
	Message string
}

type CloudJob struct {
	ID              string
	State           string
	Status          CloudStatus
	CreatedAt       int64
	CompletedAt     *int64
	Counts          *CloudCounts
	Error           *CloudError
	Labels          map[string]string
	InputURI        string
	OutputURIPrefix string
	OutputLocation  string
	expected        *int64
}

type CloudRecord struct {
	Dialect         string          `json:"dialect"`
	RouteModel      string          `json:"route_model"`
	UpstreamModel   string          `json:"upstream_model,omitempty"`
	InputURI        string          `json:"input_uri"`
	OutputURIPrefix string          `json:"output_uri_prefix"`
	ProviderJob     json.RawMessage `json:"provider_job"`
}

func (r CloudRecord) WithJob(body json.RawMessage) CloudRecord {
	r.ProviderJob = body
	return r
}

func DecodeCloudRecord(metadata []byte) (CloudRecord, error) {
	var record CloudRecord
	if json.Unmarshal(metadata, &record) != nil || len(record.ProviderJob) == 0 {
		return CloudRecord{}, errors.New("stored cloud batch metadata is malformed")
	}
	return record, nil
}

func DecodeCloudJob(adapter connectors.BatchAdapter, body []byte) (CloudJob, error) {
	switch adapter {
	case connectors.BatchVertex:
		return decodeVertexJob(body)
	case connectors.BatchBedrock:
		return decodeBedrockJob(body)
	default:
		return CloudJob{}, errors.New("unknown cloud batch adapter")
	}
}

func cloudStatus(adapter connectors.BatchAdapter, state string) (CloudStatus, error) {
	status, ok := cloudStatuses[adapter][state]
	if !ok {
		return "", ErrProviderConflict
	}
	return status, nil
}

func completedAt(status CloudStatus, values ...json.RawMessage) *int64 {
	if !status.Terminal() {
		return nil
	}
	return optionalUnix(values...)
}

func cloudError(code providerCode, message string) *CloudError {
	switch {
	case message == "":
		return nil
	case code == "":
		return &CloudError{Code: "provider_error", Message: message}
	default:
		return &CloudError{Code: string(code), Message: message}
	}
}

func (j CloudJob) snapshot(body []byte) Snapshot {
	snapshot := Snapshot{ID: j.ID, Status: j.State, Body: body, Terminal: j.Status.Terminal()}
	if snapshot.Terminal {
		snapshot.Expected = j.expected
		snapshot.OutputFileID = j.OutputLocation
	}
	return snapshot
}

func cloudAdoption(adapter connectors.BatchAdapter, settlement batches.Settlement, snapshot Snapshot, inputURI, outputURI string) (Adoption, error) {
	job, err := DecodeCloudJob(adapter, snapshot.Body)
	if err != nil {
		return Adoption{}, err
	}
	metadata, err := json.Marshal(CloudRecord{
		Dialect: adapter.CloudDialect(), RouteModel: settlement.Binding.RouteSlug, UpstreamModel: settlement.Model(),
		InputURI: inputURI, OutputURIPrefix: outputURI, ProviderJob: snapshot.Body,
	})
	return Adoption{State: string(job.Status), Metadata: metadata}, err
}

func optionalUnix(values ...json.RawMessage) *int64 {
	for _, raw := range values {
		if seconds, ok := UnixSeconds(raw); ok {
			return &seconds
		}
	}
	return nil
}

func lastSegment(value string) string {
	return value[strings.LastIndex(value, "/")+1:]
}

type providerCode string

func (c *providerCode) UnmarshalJSON(raw []byte) error {
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		*c = providerCode(number)
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	*c = providerCode(text)
	return nil
}

func cloudCounts(counts CloudCounts) *CloudCounts {
	if counts == (CloudCounts{}) {
		return nil
	}
	return &counts
}
