package gateway

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
)

func TestCloudBatchValidationAndProjection(t *testing.T) {
	storage, err := connectors.NormalizeBatchStorage("vertex_ai", "vertex-batch-prediction", &connectors.BatchStorage{
		InputURIPrefix: "gs://batch-data/in", OutputURIPrefix: "gs://batch-data/out",
	})
	if err != nil {
		t.Fatal(err)
	}
	request, failure := validateCloudBatchRequest([]byte(`{"input_uri":"gs://batch-data/in/a.jsonl","output_uri_prefix":"gs://batch-data/out/job/"}`), "vertex-batch-prediction", storage)
	if failure != nil || request.InputURI == "" {
		t.Fatalf("valid Vertex batch request=%+v error=%v", request, failure)
	}
	for _, body := range []string{
		`{"input_uri":"gs://outside/in.jsonl","output_uri_prefix":"gs://batch-data/out/"}`,
		`{"input_uri":"gs://batch-data/in/a.jsonl","output_uri_prefix":"gs://batch-data/out/","extra":true}`,
		`{"input_uri":"gs://batch-data/in/a.jsonl","output_uri_prefix":"gs://batch-data/out/","timeout_hours":24}`,
	} {
		if _, failure := validateCloudBatchRequest([]byte(body), "vertex-batch-prediction", storage); failure == nil {
			t.Fatalf("accepted invalid Vertex batch body %s", body)
		}
	}
	response, err := projectCloudBatchJob([]byte(`{"name":"projects/p/locations/us/batchPredictionJobs/upstream","state":"JOB_STATE_SUCCEEDED","createTime":"2026-10-21T12:00:00Z","endTime":"2026-10-21T13:00:00Z","completionStats":{"successfulCount":"4","failedCount":"1","incompleteCount":"0"}}`),
		"cloud_batch_0123456789abcdef0123456789abcdef", "vertex-batch-prediction", "route-model", request.InputURI, request.OutputURIPrefix)
	if err != nil {
		t.Fatal(err)
	}
	var projected map[string]any
	if json.Unmarshal(response, &projected) != nil || projected["status"] != "succeeded" || projected["model"] != "route-model" ||
		projected["input_uri"] != request.InputURI {
		t.Fatalf("cloud batch projection=%s", response)
	}
	if _, err := projectCloudBatchJob([]byte(`{"name":"projects/p/locations/us/batchPredictionJobs/id","state":"JOB_STATE_UNKNOWN"}`),
		"cloud_batch_0123456789abcdef0123456789abcdef", "vertex-batch-prediction", "route", "", ""); err == nil {
		t.Fatal("accepted an unknown Vertex batch state")
	}
}
