//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
)

type cloudFixtureRoundTripper func(*http.Request) (*http.Response, error)

func (f cloudFixtureRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type cloudFixtureReader struct {
	reader batches.Reader
	client *http.Client
}

func (r cloudFixtureReader) target(target *batches.PinnedTarget) *batches.PinnedTarget {
	copy := *target
	copy.Client = r.client
	return &copy
}
func (r cloudFixtureReader) Poll(ctx context.Context, settlement batches.Settlement, target *batches.PinnedTarget) (batches.BatchSnapshot, error) {
	return r.reader.Poll(ctx, settlement, r.target(target))
}
func (r cloudFixtureReader) Reconcile(ctx context.Context, settlement batches.Settlement, target *batches.PinnedTarget, input, endpoint string) (batches.Reconciliation, error) {
	return r.reader.Reconcile(ctx, settlement, r.target(target), input, endpoint)
}
func (r cloudFixtureReader) ReadResults(ctx context.Context, target *batches.PinnedTarget, snapshot batches.BatchSnapshot, cursor batches.Cursor, commit batches.ResultPageCommit) (batches.Cursor, bool, error) {
	return r.reader.ReadResults(ctx, r.target(target), snapshot, cursor, commit)
}

func responseForCloudFixture(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func cloudBatchUUID(t *testing.T, localID string) uuid.UUID {
	t.Helper()
	raw := strings.TrimPrefix(localID, "cloud_batch_")
	parsed, err := uuid.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestCloudBatchCreateRefreshCancelFixturesCompile(t *testing.T) {
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")
		if strings.HasSuffix(r.URL.Path, "/token") {
			writeJSON(w, map[string]any{"access_token": "fixture-token", "token_type": "Bearer", "expires_in": 3600})
			return
		}
		_, _ = w.Write([]byte("fixture-project"))
	}))
	defer metadata.Close()
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadata.URL, "http://"))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/publishers/google/models"):
			writeJSON(w, map[string]any{"publisherModels": []any{map[string]any{"name": "publishers/google/models/gemini-2.5-flash"}}})
		case r.URL.Path == "/foundation-models":
			writeJSON(w, map[string]any{"modelSummaries": []any{map[string]any{"modelId": "amazon.nova-lite-v1:0"}}})
		case strings.Contains(r.URL.Path, "batchPredictionJobs"):
			if r.Method == http.MethodPost {
				writeJSON(w, map[string]any{"name": "projects/p/locations/us-central1/batchPredictionJobs/fixture-job", "state": "JOB_STATE_QUEUED"})
			} else if strings.HasSuffix(r.URL.Path, ":cancel") {
				w.WriteHeader(http.StatusOK)
			} else if strings.Contains(r.URL.Path, "fixture-job") {
				writeJSON(w, map[string]any{"name": "projects/p/locations/us-central1/batchPredictionJobs/fixture-job", "state": "JOB_STATE_RUNNING"})
			} else {
				writeJSON(w, map[string]any{"batchPredictionJobs": []any{}})
			}
		case r.URL.Path == "/model-invocation-jobs":
			writeJSON(w, map[string]any{"invocationJobSummaries": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/model-invocation-job":
			writeJSON(w, map[string]any{"jobArn": "arn:aws:bedrock:us-east-1:123456789012:model-invocation-job/fixture-job"})
		case strings.HasSuffix(r.URL.Path, "/stop"):
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(r.URL.Path, "/model-invocation-job/"):
			writeJSON(w, map[string]any{"jobArn": "arn:aws:bedrock:us-east-1:123456789012:model-invocation-job/fixture-job", "status": "InProgress"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer fixture.Close()
	resultClient := &http.Client{Transport: cloudFixtureRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "batchPredictionJobs/fixture-job"):
			return responseForCloudFixture(req, http.StatusOK, `{"name":"projects/fixture-project/locations/us-central1/batchPredictionJobs/fixture-job","state":"JOB_STATE_SUCCEEDED","completionStats":{"successfulCount":"1","failedCount":"1","incompleteCount":"0"},"outputInfo":{"gcsOutputDirectory":"gs://fixture-bucket/output/fixture-job/"}}`), nil
		case req.URL.Host == "storage.googleapis.com" && req.URL.Path == "/storage/v1/b/fixture-bucket/o":
			return responseForCloudFixture(req, http.StatusOK, `{"items":[{"name":"output/fixture-job/prediction-0.jsonl","generation":"1"}]}`), nil
		case req.URL.Host == "storage.googleapis.com" && strings.Contains(req.URL.Path, "prediction-0.jsonl"):
			return responseForCloudFixture(req, http.StatusOK, `{"status":"","response":{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}}`+"\n"+
				`{"status":"INVALID_ARGUMENT","response":{}}`+"\n"), nil
		case strings.Contains(req.URL.Path, "model-invocation-job/fixture-job"):
			return responseForCloudFixture(req, http.StatusOK, `{"jobArn":"arn:aws:bedrock:us-east-1:123456789012:model-invocation-job/fixture-job","status":"Completed","totalRecordCount":2,"processedRecordCount":2,"successRecordCount":1,"errorRecordCount":1,"outputDataConfig":{"s3OutputDataConfig":{"s3Uri":"s3://fixture-bucket/output/"}}}`), nil
		case strings.HasPrefix(req.URL.Host, "fixture-bucket.s3.us-east-1.amazonaws.com"):
			return responseForCloudFixture(req, http.StatusOK, `{"totalRecordCount":2,"processedRecordCount":2,"successRecordCount":1,"errorRecordCount":1,"inputTokenCount":5,"outputTokenCount":3}`), nil
		default:
			return responseForCloudFixture(req, http.StatusNotFound, `{}`), nil
		}
	})}
	for _, tc := range []struct {
		kind, profile, auth, region, project, model, endpoint, scheme string
		credential                                                    any
	}{
		{"vertex_ai", "vertex-batch-prediction", "adc", "us-central1", "fixture-project", "gemini-2.5-flash",
			fixture.URL + "/v1/projects/fixture-project/locations/us-central1/publishers/google", "gs", nil},
		{"bedrock", "bedrock-batch-inference", "static", "us-east-1", "", "amazon.nova-lite-v1:0",
			fixture.URL, "s3", `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789","session_token":"fixture-session"}`},
	} {
		h := newAccessHarness(t)
		root := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.TLS.Certificates[0].Certificate[0]})
		configuration := map[string]any{
			"kind": tc.kind, "auth_mode": tc.auth, "profile_id": tc.profile, "profile_revision": "1",
			"endpoint": tc.endpoint, "cloud_region": tc.region,
			"batch_storage": map[string]any{"input_uri_prefix": tc.scheme + "://fixture-bucket/input/", "output_uri_prefix": tc.scheme + "://fixture-bucket/output/"},
			"options":       map[string]any{"network": map[string]any{"trust_roots_pem": string(root)}},
		}
		if tc.project != "" {
			configuration["cloud_project"] = tc.project
		}
		if tc.kind == "bedrock" {
			configuration["batch_storage"].(map[string]any)["role_arn"] = "arn:aws:iam::123456789012:role/fixture"
		}
		owner := h.owner()
		providerRequest := map[string]any{"name": "Batch " + tc.kind, "configuration": configuration, "model": tc.model}
		if tc.credential != nil {
			providerRequest["credential"] = tc.credential
		}
		provider := h.want(owner, http.MethodPost, "/api/v1/providers", providerRequest, idem(uuid.NewString()), http.StatusCreated)
		providerPath := "/api/v1/providers/" + provider["id"].(string)
		probe := h.want(owner, http.MethodPost, providerPath+"/probe", nil, etagHeader(provider), http.StatusOK)
		if probe["succeeded"] != true {
			t.Fatalf("%s provider probe failed: %v", tc.profile, probe)
		}
		models := h.want(owner, http.MethodGet, providerPath+"/models", nil, nil, http.StatusOK)
		modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
		model := h.want(owner, http.MethodGet, providerPath+"/models/"+modelID, nil, nil, http.StatusOK)
		updated := h.want(owner, http.MethodPatch, providerPath+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": []any{
			map[string]any{"operation": "batch", "surface": "native", "mode": "unary"},
		}}, etagHeader(model), http.StatusOK)
		certified := h.want(owner, http.MethodPost, providerPath+"/models/"+modelID+"/certify", nil, etagHeader(updated), http.StatusOK)
		if certified["status"] != "certified" {
			t.Fatalf("%s batch capability was not certified: %v", tc.profile, certified)
		}
		provider = h.want(owner, http.MethodGet, providerPath, nil, nil, http.StatusOK)
		h.want(owner, http.MethodPost, providerPath+"/activate", nil, withMatch(provider, idem(uuid.NewString())), http.StatusOK)
		slug := "cloud-batch-" + uuid.NewString()[:8]
		activate := func(routeSlug, fidelity string) {
			draft := h.want(owner, http.MethodPost, "/api/v1/route-drafts", map[string]any{
				"slug": routeSlug, "operations": []string{"batch"}, "overall_timeout_ms": 10000, "max_attempts": 1,
				"fidelity": map[string]any{"mode": fidelity},
				"targets":  []any{map[string]any{"provider_id": provider["id"], "provider_model": tc.model, "priority": 0, "weight": 1, "timeout_ms": 5000}},
			}, idem(uuid.NewString()), http.StatusCreated)
			h.want(owner, http.MethodPost, "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil,
				withMatch(draft, idem(uuid.NewString())), http.StatusOK)
		}
		activate(slug, "transformed")
		strictSlug := slug + "-strict"
		activate(strictSlug, "strict")
		key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "cloud batch", "scopes": []string{"inference"}, "allow_provider_state": true, "allowed_routes": []string{slug, strictSlug}}, idem(uuid.NewString()), http.StatusCreated)["secret"].(string)
		strictKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "strict cloud batch", "scopes": []string{"inference"}, "allow_provider_state": true, "allowed_routes": []string{strictSlug}}, idem(uuid.NewString()), http.StatusCreated)["secret"].(string)
		h.refresh()
		path := "/native/" + tc.profile + "/batch-jobs"
		outOfScope := []byte(`{"input_uri":"` + tc.scheme + `://outside/input.jsonl","output_uri_prefix":"` + tc.scheme + `://fixture-bucket/output/job/"}`)
		status, response, _ := h.gatewayRaw(http.MethodPost, path, key, bytes.NewReader(outOfScope), map[string]string{"X-OLP-Route": slug, "Content-Type": "application/json"})
		if status != http.StatusBadRequest {
			t.Fatalf("%s out-of-scope URI: status=%d response=%s", tc.profile, status, response)
		}
		valid := []byte(`{"input_uri":"` + tc.scheme + `://fixture-bucket/input/input.jsonl","output_uri_prefix":"` + tc.scheme + `://fixture-bucket/output/","display_name":"fixture-job"}`)
		status, _, _ = h.gatewayRaw(http.MethodPost, path, strictKey, bytes.NewReader(valid), map[string]string{"X-OLP-Route": strictSlug, "Content-Type": "application/json"})
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("%s strict cloud batch status=%d, want 422", tc.profile, status)
		}
		status, response, _ = h.gatewayRaw(http.MethodPost, path, key, bytes.NewReader(valid), map[string]string{"X-OLP-Route": slug, "Content-Type": "application/json"})
		if status != http.StatusOK {
			t.Fatalf("%s cloud batch create: status=%d response=%s", tc.profile, status, response)
		}
		var job map[string]any
		if json.Unmarshal(response, &job) != nil {
			t.Fatalf("%s cloud batch response is malformed: %s", tc.profile, response)
		}
		id := job["id"].(string)
		status, response, _ = h.gatewayRaw(http.MethodGet, path+"/"+id, key, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("%s cloud batch get: status=%d response=%s", tc.profile, status, response)
		}
		status, response, _ = h.gatewayRaw(http.MethodPost, path+"/"+id+"/cancel", key, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("%s cloud batch cancel: status=%d response=%s", tc.profile, status, response)
		}
		providerID := provider["id"].(string)
		h.want(owner, http.MethodPost, "/api/v1/pricing/revisions", map[string]any{
			"effective_at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
			"prices": []any{map[string]any{
				"provider_kind": tc.kind, "provider_id": providerID, "model": tc.model,
				"operation": "generation", "currency": "USD", "input_per_million": "1000000",
				"output_per_million": "1000000", "batch_multiplier": "1",
			}},
		}, idem(uuid.NewString()), http.StatusCreated)
		adapter := "vertex"
		reader := batches.Reader(&batches.VertexReader{Auth: connectors.NewAuth(&h.Policy), Egress: &h.Policy})
		if tc.kind == "bedrock" {
			adapter = "bedrock"
			reader = &batches.BedrockReader{Auth: connectors.NewAuth(&h.Policy), Egress: &h.Policy}
		}
		worker := batchWorker(h, nil)
		worker.Readers[adapter] = cloudFixtureReader{reader: reader, client: resultClient}
		pass, err := worker.ReconcileOnce(t.Context(), batches.WorkerBatchSize)
		if err != nil || pass.Settled != 1 {
			t.Fatalf("%s fixture settlement pass=%+v err=%v", tc.profile, pass, err)
		}
		counts, err := batches.NewStore(h.Pool).ResultCounts(t.Context(), cloudBatchUUID(t, id))
		resultKind := batches.ResultSucceeded
		if tc.kind == "bedrock" {
			resultKind = batches.ResultAggregate
		}
		if err != nil || counts[resultKind] != 1 || tc.kind == "vertex_ai" && counts[batches.ResultErrored] != 1 {
			t.Fatalf("%s settlement counts=%v err=%v", tc.profile, counts, err)
		}
		var facts int
		if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.attempt_usage_facts f JOIN olp.batch_settlement_results r ON r.event_id=f.event_id WHERE r.resource_id=$1`, cloudBatchUUID(t, id)).Scan(&facts); err != nil || facts != 1 {
			t.Fatalf("%s aggregate/line fact count=%d err=%v", tc.profile, facts, err)
		}
	}
}
