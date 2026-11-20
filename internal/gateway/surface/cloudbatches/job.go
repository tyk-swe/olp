package cloudbatches

import (
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
)

type object struct {
	ID              string                `json:"id"`
	Object          string                `json:"object"`
	Dialect         string                `json:"dialect"`
	Model           string                `json:"model"`
	Status          upstream.CloudStatus  `json:"status"`
	InputURI        string                `json:"input_uri"`
	OutputURIPrefix string                `json:"output_uri_prefix"`
	CreatedAt       int64                 `json:"created_at"`
	CompletedAt     *int64                `json:"completed_at"`
	RequestCounts   *upstream.CloudCounts `json:"request_counts"`
	Error           *jobError             `json:"error"`
}

type jobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func project(job upstream.CloudJob, localID string, record upstream.CloudRecord) json.RawMessage {
	return kernel.MustJSON(object{
		ID: localID, Object: "batch_job", Dialect: record.Dialect, Model: record.RouteModel, Status: job.Status,
		InputURI: record.InputURI, OutputURIPrefix: record.OutputURIPrefix, CreatedAt: job.CreatedAt,
		CompletedAt: job.CompletedAt, RequestCounts: job.Counts, Error: projectError(job.Error),
	})
}

func projectError(failure *upstream.CloudError) *jobError {
	if failure == nil {
		return nil
	}
	return &jobError{Code: failure.Code, Message: sanitize(failure.Message)}
}

var resourceARN = regexp.MustCompile(`arn:[A-Za-z0-9-:]+:[^ \t\r\n"]+`)

var resourceJob = regexp.MustCompile(`projects/[^ \t\r\n"]+/batchPredictionJobs/[^ \t\r\n"]+`)

func sanitize(message string) string {
	return resourceJob.ReplaceAllString(resourceARN.ReplaceAllString(message, "[provider resource]"), "[provider resource]")
}

func listed(row *resources.Resource) (json.RawMessage, *fault.Error) {
	record, err := upstream.DecodeCloudRecord(row.Metadata)
	adapter, cloud := connectors.CloudBatchAdapter(record.Dialect)
	if err != nil || !cloud {
		return nil, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A cloud batch could not be projected.")
	}
	job, err := upstream.DecodeCloudJob(adapter, record.ProviderJob)
	if err != nil || job.ID != row.UpstreamID {
		return nil, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A stored cloud batch has a mismatched job identity.")
	}
	return project(job, row.ID, record), nil
}
