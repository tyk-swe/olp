package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/routing"
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

func TestCloudBatchStrictTargetRefusal(t *testing.T) {
	for _, kind := range []string{"vertex_ai", "bedrock"} {
		refusal := strictBatchTargetRefusal(&pin{provider: routing.Provider{Kind: kind}})
		if refusal == nil || refusal.Status != 422 || refusal.Type != "invalid_request_error" || refusal.Code != "target_capability" {
			t.Fatalf("%s strict refusal=%+v", kind, refusal)
		}
	}
}

func TestCloudBatchCreationChecksRouteAuthority(t *testing.T) {
	h := newHarness(t, Config{})
	route := h.rt.release.Snapshot.Routes[routeSlug]
	route.Operations = []string{"batch"}
	h.rt.release.Snapshot.Routes[routeSlug] = route
	req := httptest.NewRequest(http.MethodPost, "/native/vertex-batch-prediction/batch-jobs", strings.NewReader(`{}`))
	req.SetPathValue("dialect", "vertex-batch-prediction")
	req.Header.Set("Authorization", "Bearer "+otherKey)
	req.Header.Set(stateRouteHeader, routeSlug)
	response := httptest.NewRecorder()
	h.gateway.createCloudBatch(response, req)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "route_forbidden") {
		t.Fatalf("unauthorized cloud route was admitted: %d %s", response.Code, response.Body.String())
	}
}

func TestBedrockProjectionUsesNativeCountsAndSubmitTime(t *testing.T) {
	body := []byte(`{"jobArn":"arn:aws:bedrock:us-east-1:123:model-invocation-job/job","status":"Completed","submitTime":1792497600.75,"totalRecordCount":5,"processedRecordCount":5,"successRecordCount":4,"errorRecordCount":1}`)
	projected, err := projectCloudBatchJob(body, "cloud_batch_local", "bedrock-batch-inference", "route", "s3://input/", "s3://output/")
	var result struct {
		Created int64            `json:"created_at"`
		Counts  map[string]int64 `json:"request_counts"`
	}
	if err != nil || json.Unmarshal(projected, &result) != nil || result.Created != 1792497600 || result.Counts["total"] != 5 || result.Counts["successful"] != 4 || result.Counts["failed"] != 1 {
		t.Fatalf("Bedrock native progress or timestamp was lost: %s err=%v", projected, err)
	}
}
