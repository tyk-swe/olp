package batches

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/usage"
)

func cloudBatchFixture(t *testing.T, vendor, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "vendors", vendor, name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

type cloudRoundTripper func(*http.Request) (*http.Response, error)

func (f cloudRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func cloudResponse(req *http.Request, status int, contentType string, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: http.Header{"Content-Type": []string{contentType}},
		Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func vertexTarget(t *testing.T, roundTrip cloudRoundTripper) (*VertexReader, *PinnedTarget) {
	t.Helper()
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "missing metadata flavor", http.StatusBadRequest)
			return
		}
		w.Header().Set("Metadata-Flavor", "Google")
		if strings.HasSuffix(r.URL.Path, "/token") {
			_, _ = io.WriteString(w, `{"access_token":"fixture-token","token_type":"Bearer","expires_in":3600}`)
			return
		}
		_, _ = io.WriteString(w, "project")
	}))
	t.Cleanup(metadata.Close)
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadata.URL, "http://"))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	policy := &egress.Policy{}
	cfg := connectors.Config{Kind: "vertex_ai", VendorID: "google-vertex", AuthMode: "adc", ProfileID: "vertex-batch-prediction", ProfileRevision: "1", CloudProject: "project", CloudRegion: "us-central1",
		Endpoint:     "https://us-central1-aiplatform.googleapis.com/v1/projects/project/locations/us-central1/publishers/google",
		BatchStorage: &connectors.BatchStorage{InputURIPrefix: "gs://batch-data/input/", OutputURIPrefix: "gs://batch-data/output/"}}
	return &VertexReader{Auth: connectors.NewAuth(policy), Egress: policy}, &PinnedTarget{Config: cfg, Secret: nil, Client: &http.Client{Transport: roundTrip}}
}

func TestVertexReaderPollStorageLinesAndCursorGeneration(t *testing.T) {
	job := cloudBatchFixture(t, "vertex", "job.succeeded.json")
	predictions := cloudBatchFixture(t, "vertex", "predictions.jsonl")
	reader, target := vertexTarget(t, func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "/batchPredictionJobs") && !strings.Contains(req.URL.Path, "job123"):
			return cloudResponse(req, 200, "application/json", string(cloudBatchFixture(t, "vertex", "jobs.list.response.json"))), nil
		case strings.Contains(req.URL.Path, "batchPredictionJobs/job123"):
			return cloudResponse(req, 200, "application/json", string(job)), nil
		case req.URL.Host == "storage.googleapis.com" && req.URL.Path == "/storage/v1/b/batch-data/o":
			return cloudResponse(req, 200, "application/json", `{"items":[{"name":"output/job123/prediction-1.jsonl","generation":"8"},{"name":"output/job123/ignore.txt","generation":"1"},{"name":"output/job123/other.jsonl","generation":"2"}]}`), nil
		case req.URL.Host == "storage.googleapis.com" && strings.Contains(req.URL.Path, "prediction-1.jsonl"):
			if req.URL.Query().Get("generation") != "8" {
				t.Errorf("missing generation query: %s", req.URL.RawQuery)
			}
			return cloudResponse(req, 200, "application/jsonl", string(predictions)), nil
		default:
			t.Errorf("unexpected cloud request %s", req.URL.String())
			return cloudResponse(req, 404, "application/json", "{}"), nil
		}
	})
	snapshot, err := reader.Poll(t.Context(), Settlement{UpstreamID: ptrString("job123")}, target)
	if err != nil || snapshot.Expected == nil || *snapshot.Expected != 2 || !snapshot.Terminal {
		t.Fatalf("Vertex snapshot=%+v err=%v", snapshot, err)
	}
	var results []Result
	cursor, done, err := reader.ReadResults(t.Context(), target, snapshot, Cursor{}, func(page []Result, next Cursor) error { results = append(results, page...); return nil })
	if err != nil || !done || len(results) != 2 || results[0].CustomID != "output/job123/prediction-1.jsonl#8:1" || results[1].Kind != ResultErrored || cursor.Phase != "done" {
		t.Fatalf("Vertex results=%+v cursor=%+v done=%v err=%v", results, cursor, done, err)
	}
	for _, result := range results {
		if !json.Valid(result.Evidence) {
			t.Fatalf("Vertex result is missing canonical ledger evidence: %+v", result)
		}
	}
	reconcileCursor := json.RawMessage(`{"submission":{"vertex_label":"0123456789abcdef0123456789abcdef"}}`)
	reconciled, err := reader.Reconcile(t.Context(), Settlement{Cursor: reconcileCursor}, target, "", "")
	if err != nil || len(reconciled.Matches) != 1 || reconciled.Matches[0].ID != "job123" {
		t.Fatalf("Vertex label reconciliation=%+v err=%v", reconciled, err)
	}
	_, _, err = reader.ReadResults(t.Context(), target, snapshot, Cursor{Phase: "output", File: "output/job123/prediction-1.jsonl#7", Line: 1, PrefixSHA256: "prefix"}, func([]Result, Cursor) error { return nil })
	if err != ErrCursorConflict {
		t.Fatalf("generation change error=%v", err)
	}
	badTarget := *target
	badStorage := *target.Config.BatchStorage
	badStorage.OutputURIPrefix = "gs://other-data/out/"
	badTarget.Config.BatchStorage = &badStorage
	if _, _, err := reader.ReadResults(t.Context(), &badTarget, snapshot, Cursor{}, func([]Result, Cursor) error { return nil }); !errors.Is(err, ErrStorageScope) {
		t.Fatalf("out-of-prefix Vertex result error=%v", err)
	}
}

func ptrString(value string) *string { return &value }

func TestVertexLinesCountsAndStorageScope(t *testing.T) {
	line, err := parseVertexLine([]byte(`{"status":"","response":{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}}`), "prediction-1.jsonl", "7", 4, "gemini")
	if err != nil || line.CustomID != "prediction-1.jsonl#7:4" || line.Kind != ResultSucceeded || !line.Usage.Observed {
		t.Fatalf("Vertex result=%+v err=%v", line, err)
	}
	failed, err := parseVertexLine([]byte(`{"status":"INVALID_ARGUMENT","response":{}}`), "prediction-1.jsonl", "7", 5, "gemini")
	if err != nil || failed.Kind != ResultErrored || failed.Usage.Observed {
		t.Fatalf("Vertex error=%+v err=%v", failed, err)
	}
	if counts := vertexCounts(map[string]json.RawMessage{"completionStats": json.RawMessage(`{"successfulCount":"2","failedCount":"1","incompleteCount":"3"}`)}); counts["successful"]+counts["failed"]+counts["incomplete"] != 6 {
		t.Fatalf("Vertex completion counts=%v", counts)
	}
	storage, _ := connectors.NormalizeBatchStorage("vertex_ai", "vertex-batch-prediction", &connectors.BatchStorage{InputURIPrefix: "gs://batch/in/", OutputURIPrefix: "gs://batch/out/"})
	if !storage.Contains(storage.OutputURIPrefix, "gs://batch/out/job/") || storage.Contains(storage.OutputURIPrefix, "gs://batch/outside/job/") {
		t.Fatal("Vertex output URI containment is not prefix-scoped")
	}
}

func TestBedrockManifestAggregationAndLocation(t *testing.T) {
	const arn = "arn:aws:bedrock:us-east-1:123456789012:model-invocation-job/abc123"
	if got := bedrockOutputLocation("s3://batch/output/", arn); got != "s3://batch/output/abc123/" {
		t.Fatalf("Bedrock output location=%q", got)
	}
	complete, err := parseBedrockManifest([]byte(`{"totalRecordCount":2,"processedRecordCount":2,"inputTokenCount":5,"outputTokenCount":3}`))
	if err != nil || complete.Kind != ResultAggregate || complete.CustomID != "manifest" || !complete.Usage.Observed || !complete.Usage.Complete ||
		complete.Usage.InputTokens == nil || *complete.Usage.InputTokens != 5 || complete.Usage.OutputTokens == nil || *complete.Usage.OutputTokens != 3 {
		t.Fatalf("complete Bedrock aggregate=%+v err=%v", complete, err)
	}
	partial, err := parseBedrockManifest([]byte(`{"totalRecordCount":3,"processedRecordCount":1,"inputTokenCount":2,"outputTokenCount":1}`))
	if err != nil || partial.Usage.Complete || !partial.Usage.BillingUncertain {
		t.Fatalf("partial Bedrock aggregate=%+v err=%v", partial, err)
	}
}

func TestBedrockReaderSigV4ManifestAndIdempotentReissue(t *testing.T) {
	const arn = "arn:aws:bedrock:us-east-1:123456789012:model-invocation-job/0123456789ab"
	job := cloudBatchFixture(t, "bedrock", "job.succeeded.json")
	manifest := cloudBatchFixture(t, "bedrock", "manifest.json.out")
	createBody := cloudBatchFixture(t, "bedrock", "job.create.request.json")
	var manifestRead, createRead bool
	target := &PinnedTarget{
		Config: connectors.Config{Kind: "bedrock", VendorID: "amazon-bedrock", AuthMode: "static", ProfileID: "bedrock-batch-inference", ProfileRevision: "1",
			CloudRegion: "us-east-1", Endpoint: "https://bedrock-runtime.us-east-1.amazonaws.com",
			BatchStorage: &connectors.BatchStorage{InputURIPrefix: "s3://batch/input/", OutputURIPrefix: "s3://batch/output/", RoleARN: "arn:aws:iam::123456789012:role/batch"}},
		Secret: []byte(`{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789","session_token":"bedrock-session-token"}`),
		Client: &http.Client{Transport: cloudRoundTripper(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "model-invocation-job/0123456789ab"):
				if !strings.Contains(req.Header.Get("Authorization"), "/us-east-1/bedrock/aws4_request") {
					t.Errorf("Bedrock control request was not signed for bedrock: %s", req.Header.Get("Authorization"))
				}
				return cloudResponse(req, http.StatusOK, "application/json", string(job)), nil
			case req.URL.Host == "batch.s3.us-east-1.amazonaws.com":
				manifestRead = true
				if !strings.Contains(req.Header.Get("Authorization"), "/us-east-1/s3/aws4_request") {
					t.Errorf("S3 manifest was not signed for s3: %s", req.Header.Get("Authorization"))
				}
				return cloudResponse(req, http.StatusOK, "application/json", string(manifest)), nil
			case req.Method == http.MethodPost && req.URL.Path == "/model-invocation-job":
				createRead = true
				if !strings.Contains(req.Header.Get("Authorization"), "/us-east-1/bedrock/aws4_request") {
					t.Errorf("Bedrock create request was not signed for bedrock: %s", req.Header.Get("Authorization"))
				}
				return cloudResponse(req, http.StatusOK, "application/json", string(cloudBatchFixture(t, "bedrock", "job.create.response.json"))), nil
			default:
				t.Errorf("unexpected Bedrock fixture request %s", req.URL.String())
				return cloudResponse(req, http.StatusNotFound, "application/json", "{}"), nil
			}
		})},
	}
	reader := &BedrockReader{Auth: connectors.NewAuth(&egress.Policy{}), Egress: &egress.Policy{}}
	snapshot, err := reader.Poll(t.Context(), Settlement{UpstreamID: ptrString(arn)}, target)
	if err != nil || !snapshot.Terminal || snapshot.Expected == nil || *snapshot.Expected != 1 {
		t.Fatalf("Bedrock poll=%+v err=%v", snapshot, err)
	}
	var results []Result
	_, done, err := reader.ReadResults(t.Context(), target, snapshot, Cursor{}, func(page []Result, _ Cursor) error { results = append(results, page...); return nil })
	if err != nil || !done || !manifestRead || len(results) != 1 || results[0].Kind != ResultAggregate || !results[0].Usage.Complete {
		t.Fatalf("Bedrock aggregate=%+v done=%v err=%v", results, done, err)
	}
	if !json.Valid(results[0].Evidence) {
		t.Fatalf("Bedrock manifest is missing canonical ledger evidence: %+v", results[0])
	}
	cursor, _ := json.Marshal(map[string]any{"submission": map[string]any{
		"bedrock_client_request_token": "01234567-89ab-cdef-0123-456789abcdef", "create_body": json.RawMessage(createBody),
	}})
	reconciliation, err := reader.Reconcile(t.Context(), Settlement{Cursor: cursor}, target, "", "")
	if err != nil || len(reconciliation.Matches) != 1 || reconciliation.Matches[0].ID != arn || !createRead {
		t.Fatalf("Bedrock reconcile=%+v err=%v", reconciliation, err)
	}
	if bedrockReissueNoEvidence(http.StatusConflict, []byte(`{"__type":"ConflictException"}`)) != true ||
		bedrockReissueNoEvidence(http.StatusBadRequest, []byte(`{"__type":"ValidationException"}`)) != true ||
		bedrockReissueNoEvidence(http.StatusInternalServerError, []byte(`{"__type":"InternalServerException"}`)) {
		t.Fatal("Bedrock idempotent create reissue conflict decision changed")
	}
}

func TestCloudReadersRegisteredAndSubmissionAdoptionMetadata(t *testing.T) {
	w := &Worker{}
	w.setup()
	if w.Readers["vertex"] == nil || w.Readers["bedrock"] == nil {
		t.Fatal("cloud batch settlement readers were not registered")
	}
	job := []byte(`{"name":"projects/p/locations/us/batchPredictionJobs/job","state":"JOB_STATE_RUNNING","inputConfig":{"instancesFormat":"jsonl","gcsSource":{"uris":["gs://batch/in.jsonl"]}},"outputConfig":{"gcsDestination":{"outputUriPrefix":"gs://batch/out/"}}}`)
	model := "model"
	settlement := Settlement{Adapter: "vertex", ResourceID: uuid.New(), RouteSlug: "route", Template: &usage.Event{UpstreamModel: &model}}
	meta, state, err := cloudAdoptMetadata(settlement, BatchSnapshot{ID: "job", Body: job})
	if err != nil || state != "running" {
		t.Fatalf("Vertex adopted state=%s err=%v", state, err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(meta, &fields) != nil {
		t.Fatal("Vertex adoption metadata is invalid")
	}
	var input, output string
	_ = json.Unmarshal(fields["input_uri"], &input)
	_ = json.Unmarshal(fields["output_uri_prefix"], &output)
	if input != "gs://batch/in.jsonl" || output != "gs://batch/out/" {
		t.Fatalf("Vertex adoption URIs=%s %s", input, output)
	}
}

func TestCloudReconciliationGuards(t *testing.T) {
	if !sameVertexObjectGeneration("prediction.jsonl", "7", gcsObject{Name: "prediction.jsonl", Generation: "7"}) ||
		sameVertexObjectGeneration("prediction.jsonl", "7", gcsObject{Name: "prediction.jsonl", Generation: "8"}) {
		t.Fatal("Vertex cursor generation guard failed")
	}
	if !vertexSubmissionLabelMatches(map[string]string{"olp_submission": "abc"}, "abc") ||
		vertexSubmissionLabelMatches(map[string]string{"olp_submission": "other"}, "abc") {
		t.Fatal("Vertex label reconcile filter failed")
	}
	if bedrockCreateReissueHasEvidence(409, []byte(`{"__type":"ConflictException"}`)) ||
		bedrockCreateReissueHasEvidence(400, []byte(`{"__type":"ValidationException"}`)) ||
		!bedrockCreateReissueHasEvidence(500, []byte(`{"__type":"InternalServerException"}`)) {
		t.Fatal("Bedrock idempotent reissue classification failed")
	}
}

func TestVertexReconcileTerminalOutput(t *testing.T) {
	var job map[string]json.RawMessage
	if err := json.Unmarshal(cloudBatchFixture(t, "vertex", "job.succeeded.json"), &job); err != nil {
		t.Fatal(err)
	}
	job["labels"] = json.RawMessage(`{"olp_submission":"submission-label"}`)
	listing, _ := json.Marshal(map[string]any{"batchPredictionJobs": []any{job}})
	reader, target := vertexTarget(t, func(req *http.Request) (*http.Response, error) {
		return cloudResponse(req, 200, "application/json", string(listing)), nil
	})
	matches, err := reader.Reconcile(t.Context(), Settlement{Cursor: []byte(`{"submission":{"vertex_label":"submission-label"}}`)}, target, "", "")
	if err != nil || len(matches.Matches) != 1 || !matches.Matches[0].Terminal || matches.Matches[0].OutputFileID != "gs://batch-data/output/job123/" {
		t.Fatalf("terminal adoption lost its GCS output identity: %+v err=%v", matches, err)
	}
}

func TestBedrockManifestRequiresUsageCounters(t *testing.T) {
	for _, body := range []string{
		`{"totalRecordCount":2,"processedRecordCount":2}`,
		`{"totalRecordCount":2,"processedRecordCount":2,"inputTokenCount":0}`,
		`{"totalRecordCount":2,"processedRecordCount":2,"inputTokenCount":0,"outputTokenCount":null}`,
	} {
		if _, err := parseBedrockManifest([]byte(body)); err == nil {
			t.Fatalf("absent usage was treated as zero: %s", body)
		}
	}
	zero, err := parseBedrockManifest([]byte(`{"totalRecordCount":0,"processedRecordCount":0,"inputTokenCount":0,"outputTokenCount":0}`))
	if err != nil || !zero.Usage.Complete || zero.Usage.InputTokens == nil || *zero.Usage.InputTokens != 0 {
		t.Fatalf("explicit zero usage is invalid: %+v err=%v", zero, err)
	}
}

func TestVertexOmittedZeroCountsAndDottedS3Bucket(t *testing.T) {
	counts := vertexCounts(map[string]json.RawMessage{"completionStats": json.RawMessage(`{"successfulCount":"2"}`)})
	if counts == nil || counts["successful"] != 2 || counts["failed"] != 0 || counts["incomplete"] != 0 {
		t.Fatalf("omitted protobuf zero counts became unknown: %v", counts)
	}
	if invalid := vertexCounts(map[string]json.RawMessage{"completionStats": json.RawMessage(`{"successfulCount":"2","failedCount":null}`)}); invalid != nil {
		t.Fatalf("malformed supplied count became zero: %v", invalid)
	}
	if got := bedrockManifestURL("batch.results", "output/job/manifest.json.out", "us-east-1"); got != "https://s3.us-east-1.amazonaws.com/batch.results/output/job/manifest.json.out" {
		t.Fatalf("dotted bucket uses a TLS-incompatible hostname: %s", got)
	}
}
