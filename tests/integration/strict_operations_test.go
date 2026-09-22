//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
)

type operationFixture struct {
	*httptest.Server
	mu           sync.Mutex
	calls        []strictProviderCall
	response     string
	expectedPath string
	policy       any
}

func newOperationFixture(t *testing.T, path, response string) *operationFixture {
	t.Helper()
	f := &operationFixture{response: response, expectedPath: path}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, strictProviderCall{body, r.Header.Clone(), r.URL.Query()})
		response := f.response
		f.mu.Unlock()
		if r.URL.Path != path {
			t.Errorf("native path %s, want %s", r.URL.Path, path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(f.Close)
	return f
}
func (f *operationFixture) snapshot() []strictProviderCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]strictProviderCall(nil), f.calls...)
}
func (f *operationFixture) result(value string) { f.mu.Lock(); defer f.mu.Unlock(); f.response = value }
func publishOperation(t *testing.T, h *accessHarness, owner *browser, f *operationFixture, profileID, operation, surface string, defaults map[string]any) (string, string) {
	t.Helper()
	profile, err := connectors.LookupProfile(profileID, "1")
	if err != nil {
		t.Fatal(err)
	}
	configuration := map[string]any{"kind": profile.Kind, "profile_id": profile.ID, "profile_revision": "1", "endpoint": f.URL + "/v1", "auth_mode": "api_key"}
	if defaults != nil {
		configuration["options"] = map[string]any{"operation_defaults": map[string]any{operation: defaults}}
	}
	credential := vendorSecret
	if profile.Kind == "bedrock" {
		configuration["auth_mode"] = "static"
		configuration["cloud_region"] = "us-east-1"
		credential = `{"access_key_id":"FIXTUREACCESSKEY12345","secret_access_key":"fixture-only-secret"}`
	}
	providerInput := map[string]any{"name": "Operation " + uuid.NewString(), "configuration": configuration, "model": vendorModel, "credential": credential}
	if profile.Kind == "vertex_ai" {
		configuration["auth_mode"] = "adc"
		configuration["cloud_project"] = "fixture-project"
		configuration["cloud_region"] = "us-central1"
		configuration["endpoint"] = f.URL + "/v1/projects/fixture-project/locations/us-central1/publishers/google"
		delete(providerInput, "credential")
	}
	provider := h.want(owner, "POST", "/api/v3/providers", providerInput, idem(uuid.NewString()), 201)
	providerPath := "/api/v3/providers/" + provider["id"].(string)
	modelID := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)["items"].([]any)[0].(map[string]any)["id"].(string)
	provider = h.want(owner, "PATCH", providerPath+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": operationCapabilities(profileID, operation, surface)}, etagHeader(provider), 200)
	f.mu.Lock()
	originalResult := f.response
	f.mu.Unlock()
	switch profileID {
	case "openai-input-tokens":
		f.result(`{"object":"response.input_tokens","input_tokens":3}`)
	case "anthropic-messages":
		f.result(`{"input_tokens":3}`)
	case "gemini-generation":
		if operation == "embeddings" {
			f.result(`{"embedding":{"values":[1,-2]}}`)
		} else {
			f.result(`{"totalTokens":3}`)
		}
	case "gemini-batch-embeddings":
		f.result(`{"embeddings":[{"values":[1,-2]}]}`)
	case "vertex-gemini":
		f.result(`{"predictions":[{"embeddings":{"values":[1,-2],"statistics":{"token_count":3}}}]}`)
	case "bedrock-converse":
		if operation == "embeddings" {
			f.result(`{"embedding":[1,-2],"inputTextTokenCount":3}`)
		} else {
			f.result(`{"inputTokens":3}`)
		}
	case "openai-moderation":
		f.result(`{"id":"mod-probe","model":"fixture-model","results":[{"flagged":false,"categories":{"unsafe":false},"category_scores":{"unsafe":0.1}}]}`)
	case "tei-classification":
		f.result(`[{"label":"a","score":1.5}]`)
	case "tei-scoring":
		f.result(`[1.5,-0.25]`)
	case "tei-tokenize":
		f.result(`[[{"id":1,"text":"probe","special":false,"start":0,"stop":5}]]`)

	case "openai-embeddings":
		f.result(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1,-2]}],"model":"fixture-model","usage":{"prompt_tokens":2,"total_tokens":2}}`)
	case "voyage-embeddings":
		f.result(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1,2]}],"model":"fixture-model","usage":{"total_tokens":2}}`)
	case "tei-embeddings":
		f.result(`[[1,-2]]`)
	case "tei-sparse-embeddings":
		f.result(`[[{"index":1,"value":0.2}]]`)
	case "tei-multivector-embeddings":
		f.result(`[[[0.1,0.2]]]`)
	case "tei-rerank":
		f.result(`[{"index":1,"score":2.5},{"index":0,"score":-1.25}]`)
	case "rerank":
		f.result(`{"results":[{"index":1,"relevance_score":2.5},{"index":0,"relevance_score":-1.25}]}`)
	case "voyage-rerank":
		f.result(`{"data":[{"index":1,"relevance_score":2.5},{"index":0,"relevance_score":-1.25}],"usage":{"total_tokens":3}}`)
	}
	certified := h.want(owner, "POST", providerPath+"/models/"+modelID+"/certify", nil, etagHeader(provider), 200)
	if certified["status"] == "failed" {
		t.Fatalf("native certification %s: %v", profileID, certified)
	}
	provider = h.want(owner, "GET", providerPath, nil, nil, 200)
	f.mu.Lock()
	f.response = originalResult
	f.calls = nil
	f.mu.Unlock()
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(provider, idem(uuid.NewString())), 200)
	slug := "native-" + uuid.NewString()
	draftInput := fidelityDraft(slug, provider["id"])
	draftInput["operations"] = []string{operation}
	draftInput["fidelity"] = map[string]any{"mode": "strict"}
	if f.policy != nil {
		draftInput["content_policy"] = f.policy
	}
	draft := h.want(owner, "POST", "/api/v3/route-drafts", draftInput, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "Native operation", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem(uuid.NewString()), 201)
	h.refresh()
	return slug, key["secret"].(string)
}
func TestStrictNativeVectorStoragePublic(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	response := `{"object":"list","data":[{"object":"embedding","index":1,"embedding":[255,0]},{"object":"embedding","index":0,"embedding":[1,128]}],"model":"fixture-model","usage":{"total_tokens":9},"native":{"revision":9007199254740993}}`
	f := newOperationFixture(t, "/v1/embeddings", response)
	slug, key := publishOperation(t, h, owner, f, "voyage-embeddings", "embeddings", "native", map[string]any{"dialect": "voyage-embeddings", "values": map[string]any{"output_dtype": "ubinary", "truncation": false}})
	body := fmt.Sprintf(`{"model":%q,"input":["document A","document B"],"output_dimension":16,"encoding_format":null,"input_type":null,"native":{"counter":9007199254740993,"zero":-0,"nothing":null}}`, slug)
	status, raw, _ := h.gatewayRaw("POST", "/native/voyage-embeddings/models/"+slug, key, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if status != 400 || len(f.snapshot()) != 0 || !bytes.Contains(raw, []byte(`"code":"state_carrier"`)) {
		t.Fatalf("missing raw client contract dispatched: %d %s", status, raw)
	}
	status, raw, _ = h.gatewayRaw("POST", "/native/voyage-embeddings/models/"+slug, key, strings.NewReader(body), map[string]string{"Content-Type": "application/json", "X-OLP-Client-Contract": "raw-vector-storage/1"})
	if status != 200 {
		t.Fatalf("native packed result: %d %s", status, raw)
	}
	var actual map[string]json.RawMessage
	if json.Unmarshal(raw, &actual) != nil {
		t.Fatal("invalid result")
	}
	if !bytes.Contains(actual["data"], []byte(`[255,0]`)) || !bytes.Contains(actual["native"], []byte(`9007199254740993`)) {
		t.Fatalf("native vector values changed: %s", raw)
	}
	calls := f.snapshot()
	if len(calls) != 1 {
		t.Fatalf("dispatch count %d", len(calls))
	}
	var sent map[string]json.RawMessage
	_ = json.Unmarshal(calls[0].body, &sent)
	if string(sent["encoding_format"]) != "null" || string(sent["input_type"]) != "null" || string(sent["truncation"]) != "false" || !bytes.Contains(sent["native"], []byte(`"zero":-0`)) {
		t.Fatalf("native presence/defaults changed: %s", calls[0].body)
	}
}
func TestStrictNativeSparseAndMultivectorPublic(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	for _, test := range []struct{ profile, path, request, result string }{{"tei-embeddings", "embed", `{"inputs":["a","b"],"normalize":false,"dimensions":2,"prompt_name":null}`, `[[0.1,-0.2],[0.3,0.4]]`}, {"tei-sparse-embeddings", "embed_sparse", `{"inputs":["a","b"],"truncate":null,"prompt_name":null}`, `[[{"index":900,"value":-0},{"index":2,"value":1.0000001}],[{"index":1000,"value":0.2}]]`}, {"tei-multivector-embeddings", "embed_all", `{"inputs":["a","b"],"truncate":false,"truncation_direction":"Left"}`, `[[[0.1,0.2],[0.3,0.4]],[[0.5,0.6]]]`}} {
		t.Run(test.profile, func(t *testing.T) {
			f := newOperationFixture(t, "/v1/"+test.path, test.result)
			slug, key := publishOperation(t, h, owner, f, test.profile, "embeddings", "native", nil)
			headers := map[string]string{"Content-Type": "application/json"}
			if test.profile != "tei-embeddings" {
				headers["X-OLP-Client-Contract"] = "raw-vector-storage/1"
			}
			status, body, _ := h.gatewayRaw("POST", "/native/"+test.profile+"/models/"+slug, key, strings.NewReader(test.request), headers)
			if status != 200 || string(body) != test.result {
				t.Fatalf("native storage: %d %s", status, body)
			}
			if len(f.snapshot()) != 1 || string(f.snapshot()[0].body) != test.request {
				t.Fatal("native source was normalized")
			}
		})
	}
}

func TestStrictNativeHostedEmbeddingShapesPublic(t *testing.T) {
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" || !strings.HasSuffix(r.URL.Path, "/token") {
			http.Error(w, "invalid fixture metadata request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Metadata-Flavor", "Google")
		writeJSON(w, map[string]any{"access_token": "operation-adc-token", "token_type": "Bearer", "expires_in": 3600})
	}))
	defer metadata.Close()
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadata.URL, "http://"))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	h := newAccessHarness(t)
	owner := h.owner()
	for _, test := range []struct{ profile, dialect, path, request, response, clientContract string }{
		{"gemini-generation", "gemini-embeddings", "/v1/models/fixture-model:embedContent", `{"model":"models/ROUTE","content":{"parts":[{"text":"native task"}]},"outputDimensionality":2,"taskType":"RETRIEVAL_QUERY","title":null}`, `{"embedding":{"values":[0.10000000000000001,-0]},"usageMetadata":{"promptTokenCount":3},"native":{"presence":null}}`, ""},
		{"gemini-batch-embeddings", "gemini-batch-embeddings", "/v1/models/fixture-model:batchEmbedContents", `{"requests":[{"model":"models/ROUTE","content":{"parts":[{"text":"one"}]},"taskType":"RETRIEVAL_DOCUMENT","title":"title"},{"model":"models/ROUTE","content":{"parts":[{"text":"two"}]},"outputDimensionality":2}]}`, `{"embeddings":[{"values":[0.1,-0.2]},{"values":[0.3,0.4]}],"native":{"ordered":true}}`, ""},
		{"vertex-gemini", "vertex-embeddings", "/v1/projects/fixture-project/locations/us-central1/publishers/google/models/fixture-model:predict", `{"instances":[{"content":"one","task_type":"RETRIEVAL_QUERY"},{"content":"two","task_type":"RETRIEVAL_DOCUMENT"}],"parameters":{"outputDimensionality":2}}`, `{"predictions":[{"embeddings":{"values":[0.1,-0.2],"statistics":{"token_count":2}}},{"embeddings":{"values":[0.3,0.4],"statistics":{"token_count":3}}}],"metadata":{"modelRevision":"fixture-revision"}}`, ""},
		{"bedrock-converse", "bedrock-embeddings", "/v1/model/fixture-model/invoke", `{"inputText":"native binary","dimensions":8,"embeddingTypes":["binary"],"normalize":false}`, `{"embeddingsByType":{"binary":[1,0,1,0,1,0,1,0]},"inputTextTokenCount":4,"native":{"storage":"binary"}}`, "raw-vector-storage/1"},
	} {
		t.Run(test.dialect, func(t *testing.T) {
			f := newOperationFixture(t, test.path, test.response)
			slug, key := publishOperation(t, h, owner, f, test.profile, "embeddings", "native", nil)
			body := strings.ReplaceAll(test.request, "ROUTE", slug)
			headers := map[string]string{"Content-Type": "application/json"}
			if test.clientContract != "" {
				headers["X-OLP-Client-Contract"] = test.clientContract
			}
			status, output, _ := h.gatewayRaw("POST", "/native/"+test.dialect+"/models/"+slug, key, strings.NewReader(body), headers)
			if status != 200 || string(output) != test.response {
				t.Fatalf("native embedding shape changed: %d %s", status, output)
			}
			calls := f.snapshot()
			if len(calls) != 1 || string(calls[0].body) != strings.ReplaceAll(body, slug, vendorModel) {
				t.Fatalf("native task or shape controls changed: %+v", calls)
			}
			if test.profile == "vertex-gemini" && calls[0].headers.Get("Authorization") != "Bearer operation-adc-token" {
				t.Fatal("Vertex host authentication was not applied after native request construction")
			}
			if test.profile == "bedrock-converse" && !strings.Contains(calls[0].headers.Get("Authorization"), "/us-east-1/bedrock/aws4_request") {
				t.Fatal("Bedrock native embedding request was not signed for its hosting region")
			}
		})
	}
}

func TestStrictNativeRerankScoresAndIdentityPublic(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	for _, test := range []struct{ profile, body, response string }{
		{"tei-rerank", `{"query":"q","texts":["zero","one","two"],"raw_scores":true,"return_text":true,"truncate":false,"truncation_direction":"Left"}`, `[{"index":2,"score":2.5000001,"text":"two"},{"index":0,"score":2.5000001,"text":"zero"},{"index":1,"score":-3.75,"text":"one"}]`},
		{"rerank", `{"model":"ROUTE","query":"q","documents":[{"id":"original-A","text":"alpha","metadata":{"x":-0}},{"id":"original-B","text":"beta"}],"top_n":2,"return_documents":true}`, `{"results":[{"index":1,"relevance_score":1.25,"document":{"id":"original-B","text":"beta"}},{"index":0,"relevance_score":-0,"document":{"id":"original-A","text":"alpha","metadata":{"x":-0}}}],"meta":{"trace":"keep"}}`},
	} {
		t.Run(test.profile, func(t *testing.T) {
			f := newOperationFixture(t, "/v1/rerank", test.response)
			slug, key := publishOperation(t, h, owner, f, test.profile, "rerank", "native", nil)
			request := strings.ReplaceAll(test.body, "ROUTE", slug)
			status, result, _ := h.gatewayRaw("POST", "/native/"+test.profile+"/models/"+slug, key, strings.NewReader(request), map[string]string{"Content-Type": "application/json"})
			if status != 200 || string(result) != test.response {
				t.Fatalf("native ranking changed: %d %s", status, result)
			}
			calls := f.snapshot()
			if len(calls) != 1 {
				t.Fatalf("calls %d", len(calls))
			}
			expected := strings.ReplaceAll(request, slug, vendorModel)
			if string(calls[0].body) != expected {
				t.Fatalf("ranking controls changed: %s", calls[0].body)
			}
		})
	}
}
func TestStrictNativeVectorCorruptionNeverReturnsPartialSuccess(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	f := newOperationFixture(t, "/v1/embeddings", `{"data":[{"index":0,"embedding":[1]}],"usage":{"total_tokens":1}}`)
	slug, key := publishOperation(t, h, owner, f, "voyage-embeddings", "embeddings", "native", nil)
	for _, response := range []string{`{"data":[{"index":0,"embedding":[128]}]}`, `{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[1]}]}`, `{"data":[{"index":0,"embedding":[1]}],"data":[]}`, `{"data":[{"index":0,"embedding":[1]}]} {"tail":true}`} {
		f.result(response)
		before := len(f.snapshot())
		body := fmt.Sprintf(`{"model":%q,"input":"a","output_dtype":"int8"}`, slug)
		status, result, headers := h.gatewayRaw("POST", "/native/voyage-embeddings/models/"+slug, key, strings.NewReader(body), map[string]string{"Content-Type": "application/json", "X-OLP-Client-Contract": "raw-vector-storage/1"})
		if status != 502 || headers.Get("X-Should-Retry") != "false" || !bytes.Contains(result, []byte(`"code":"fidelity_protocol_violation"`)) || len(f.snapshot()) != before+1 {
			t.Fatalf("corrupt output replayed/exposed: %d %s", status, result)
		}
	}
}

func TestStrictNativeRerankCorruptionNeverReturnsPartialSuccess(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	f := newOperationFixture(t, "/v1/rerank", `[{"index":1,"score":2.5},{"index":0,"score":-1.25}]`)
	slug, key := publishOperation(t, h, owner, f, "tei-rerank", "rerank", "native", nil)
	request := `{"query":"q","texts":["first","second"],"return_text":true,"raw_scores":true}`
	for _, result := range []string{
		`[{"index":0,"score":1,"text":"first"},{"index":0,"score":2,"text":"first"}]`,
		`[{"index":0,"score":1,"text":"changed"},{"index":1,"score":2,"text":"second"}]`,
		`[{"index":0,"score":"1","text":"first"},{"index":1,"score":2,"text":"second"}]`,
	} {
		f.result(result)
		before := len(f.snapshot())
		status, output, _ := h.gatewayRaw("POST", "/native/tei-rerank/models/"+slug, key, strings.NewReader(request), map[string]string{"Content-Type": "application/json"})
		if status != 502 || !bytes.Contains(output, []byte(`"code":"fidelity_protocol_violation"`)) || len(f.snapshot()) != before+1 {
			t.Fatalf("corrupt ranking exposed as provider success: %d %s", status, output)
		}
	}
}
func TestStrictQualifiedUnaryOperationsPublic(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	f := newOperationFixture(t, "/v1/embeddings", `{"object":"list","data":[{"index":0,"embedding":[0.25,-0.5]}],"model":"fixture-model","usage":{"total_tokens":3},"metadata":{"counter":9007199254740993}}`)
	slug, key := publishOperation(t, h, owner, f, "voyage-embeddings", "embeddings", "openai", nil)
	body := fmt.Sprintf(`{"model":%q,"input":"native text","dimensions":2,"encoding_format":"float"}`, slug)
	status, result, _ := h.gatewayRaw("POST", "/v1/embeddings", key, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if status != 200 {
		t.Fatalf("qualified embedding: %d %s", status, result)
	}
	sent := f.snapshot()[0].body
	for _, part := range []string{`"output_dimension":2`, `"truncation":false`, `"encoding_format":null`} {
		if !bytes.Contains(sent, []byte(part)) {
			t.Fatalf("missing qualified control %s in %s", part, sent)
		}
	}
	if !bytes.Contains(result, []byte(`"prompt_tokens":3`)) || !bytes.Contains(result, []byte(`9007199254740993`)) {
		t.Fatalf("usage/native metadata dropped %s", result)
	}
	before := len(f.snapshot())
	body = fmt.Sprintf(`{"model":%q,"input":"a","user":"unmapped identity"}`, slug)
	status, result, _ = h.gatewayRaw("POST", "/v1/embeddings", key, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if status != 400 || len(f.snapshot()) != before {
		t.Fatalf("unmapped control dispatched: %d %s", status, result)
	}
	ranking := newOperationFixture(t, "/v1/rerank", `{"data":[{"index":1,"relevance_score":2.75,"document":"second"}],"usage":{"total_tokens":5},"metadata":{"untouched":true}}`)
	slug, key = publishOperation(t, h, owner, ranking, "voyage-rerank", "rerank", "openai", nil)
	body = fmt.Sprintf(`{"model":%q,"query":"q","documents":["first","second"],"top_n":1,"return_documents":true,"truncation":false}`, slug)
	status, result, _ = h.gatewayRaw("POST", "/v1/rerank", key, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if status != 200 || !bytes.Contains(result, []byte(`"results":[{"index":1,"relevance_score":2.75,"document":"second"}]`)) || !bytes.Contains(ranking.snapshot()[0].body, []byte(`"top_k":1`)) {
		t.Fatalf("qualified rerank: %d %s", status, result)
	}
}

func operationCapabilities(profile, operation, surface string) []any {
	out := []any{map[string]any{"operation": operation, "surface": surface, "mode": "unary"}}
	if surface == "native" && (profile == "voyage-embeddings" || profile == "voyage-rerank" || profile == "rerank") {
		out = append(out, map[string]any{"operation": operation, "surface": "openai", "mode": "unary"})
	}
	return out
}
func TestStrictOperationsPinnedSDKStorage(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	float := newOperationFixture(t, "/v1/embeddings", `{"object":"list","data":[{"object":"embedding","index":0,"embedding":"AACAPwAAAMA="}],"model":"fixture-model","usage":{"prompt_tokens":2,"total_tokens":2}}`)
	floatRoute, floatKey := publishOperation(t, h, owner, float, "openai-embeddings", "embeddings", "openai", nil)
	packed := newOperationFixture(t, "/v1/embeddings", `{"object":"list","data":[{"object":"embedding","index":0,"embedding":"AP8="}],"model":"fixture-model","usage":{"total_tokens":2}}`)
	packedRoute, packedKey := publishOperation(t, h, owner, packed, "voyage-embeddings", "embeddings", "native", map[string]any{"dialect": "voyage-embeddings", "values": map[string]any{"output_dtype": "uint8"}})
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{{"node", "tests/sdk-smoke/operation-storage.mjs"}, {"uv", "run", "--project", "tests/sdk-smoke-python", "--frozen", "python", "tests/sdk-smoke-python/operation_storage.py"}} {
		cmd := exec.CommandContext(t.Context(), command[0], command[1:]...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "OLP_OPERATION_BASE="+h.HTTP.URL, "OLP_OPERATION_FLOAT_ROUTE="+floatRoute, "OLP_OPERATION_FLOAT_KEY="+floatKey, "OLP_OPERATION_PACKED_ROUTE="+packedRoute, "OLP_OPERATION_PACKED_KEY="+packedKey)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("pinned SDK contract %s: %v\n%s", command[0], err, output)
		}
	}
	if len(float.snapshot()) != 4 || len(packed.snapshot()) != 2 {
		t.Fatalf("SDK rejection dispatched: float=%d packed=%d", len(float.snapshot()), len(packed.snapshot()))
	}
	for _, call := range float.snapshot() {
		if !bytes.Contains(call.body, []byte(`"encoding_format":"base64"`)) {
			t.Fatalf("SDK default wire serialization not exercised: %s", call.body)
		}
	}
}

func TestStrictClassificationAndNativeCountPublic(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	for _, test := range []struct{ profile, dialect, operation, path, body, result string }{
		{"openai-moderation", "openai-moderation", "moderation", "/v1/moderations", `{"model":"ROUTE","input":["one","two"]}`, `{"id":"mod-result","model":"fixture-model","results":[{"flagged":true,"categories":{"native/category":true},"category_scores":{"native/category":0.7000000000000001},"category_applied_input_types":{"native/category":["text"]},"thresholds":{"native/category":0.7}},{"flagged":false,"categories":{"native/category":false},"category_scores":{"native/category":-0},"category_applied_input_types":{"native/category":["text"]}}],"native":{"scope":"two independent texts"}}`},
		{"tei-classification", "tei-classification", "classification", "/v1/predict", `{"inputs":["premise","hypothesis"],"raw_scores":true,"truncate":null,"truncation_direction":"Left"}`, `[{"label":"same","score":2.5000001},{"label":"same","score":-3.75}]`},
		{"tei-scoring", "tei-scoring", "scoring", "/v1/similarity", `{"inputs":{"source_sentence":"q","sentences":["one","two","three"]},"parameters":{"truncate":false,"truncation_direction":"Left","prompt_name":null}}`, `[2.5000001,-0,1e1000]`},
		{"openai-input-tokens", "openai-input-tokens", "token_count", "/v1/responses/input_tokens", `{"model":"ROUTE","input":"count me","instructions":null,"parallel_tool_calls":false,"text":{"format":{"type":"text"}}}`, `{"object":"response.input_tokens","input_tokens":9007199254740993,"native":{"estimated_tokens":99}}`},
		{"anthropic-messages", "anthropic-count-tokens", "token_count", "/v1/messages/count_tokens", `{"model":"ROUTE","messages":[{"role":"user","content":[{"type":"text","text":"count me"}]}],"system":"system text","tools":[{"name":"native","description":"a tool","input_schema":{"type":"object","properties":{}}}]}`, `{"input_tokens":9007199254740993,"native":{"cache_scope":"request"}}`},
		{"gemini-generation", "gemini-count-tokens", "token_count", "/v1/models/fixture-model:countTokens", `{"generateContentRequest":{"model":"models/ROUTE","contents":[{"role":"user","parts":[{"text":"count me"}]}],"systemInstruction":{"parts":[{"text":"system"}]},"tools":[{"functionDeclarations":[{"name":"native","parameters":{"type":"OBJECT","properties":{}}}]}]}}`, `{"totalTokens":9007199254740993,"cachedContentTokenCount":7,"promptTokensDetails":[{"modality":"TEXT","tokenCount":9}]}`},
		{"bedrock-converse", "bedrock-count-tokens", "token_count", "/v1/model/fixture-model/count-tokens", `{"input":{"converse":{"messages":[{"role":"user","content":[{"text":"count me"}]}],"system":[{"text":"system"}]}}}`, `{"inputTokens":9007199254740993,"native":{"estimate":12}}`},
		{"tei-tokenize", "tei-tokenize", "token_count", "/v1/tokenize", `{"inputs":["é é",""],"add_special_tokens":false,"prompt_name":null}`, `[[{"id":4294967295,"text":"é","special":false,"start":0,"stop":1},{"id":2,"text":"é","special":false,"start":2,"stop":4}],[]]`},
	} {
		t.Run(test.dialect, func(t *testing.T) {
			f := newOperationFixture(t, test.path, test.result)
			slug, key := publishOperation(t, h, owner, f, test.profile, test.operation, "native", nil)
			body := strings.ReplaceAll(test.body, "ROUTE", slug)
			sink := make(strictOutcomeSink, 2)
			h.Gateway.Sink = sink
			status, result, _ := h.gatewayRaw("POST", "/native/"+test.dialect+"/models/"+slug, key, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
			expected := strings.ReplaceAll(test.result, `"model":"fixture-model"`, `"model":"`+slug+`"`)
			if status != 200 || string(result) != expected {
				t.Fatalf("native %s: %d %s", test.dialect, status, result)
			}
			calls := f.snapshot()
			if len(calls) != 1 || string(calls[0].body) != strings.ReplaceAll(body, slug, vendorModel) {
				t.Fatalf("native request mutated: %+v", calls)
			}
			envelope := <-sink
			if len(envelope.Attempts) != 1 || envelope.Attempts[0].Interaction == nil || envelope.Operation != test.operation || envelope.Usage != nil {
				t.Fatalf("native counts/scores entered generation usage: %+v", envelope)
			}
		})
	}
}

func TestStrictOperationPolicyRefusalPreservesUpstreamOutcome(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	scoring := newOperationFixture(t, "/v1/similarity", `[1.5,-0.25]`)
	scoring.policy = fidelityPolicy("block", "input")
	scoringRoute, scoringKey := publishOperation(t, h, owner, scoring, "tei-scoring", "scoring", "native", nil)
	for _, body := range []string{
		`{"inputs":{"source_sentence":"q","sentences":["private-marker","second"]}}`,
		`{"inputs":{"source_sentence":"q","sentences":["first","second"]},"parameters":{"prompt_name":"private-marker"}}`,
	} {
		status, output, _ := h.gatewayRaw("POST", "/native/tei-scoring/models/"+scoringRoute, scoringKey, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
		if status != 400 || len(scoring.snapshot()) != 0 || !(bytes.Contains(output, []byte(`"code":"content_policy_blocked"`)) || bytes.Contains(output, []byte(`"code":"policy_conflict"`))) {
			t.Fatalf("uninspectable or blocked input dispatched: %d %s", status, output)
		}
	}
	ranking := newOperationFixture(t, "/v1/rerank", `{"data":[{"index":0,"relevance_score":1.25,"document":"private-marker"}],"usage":{"total_tokens":3}}`)
	ranking.policy = fidelityPolicy("block", "output")
	route, key := publishOperation(t, h, owner, ranking, "voyage-rerank", "rerank", "native", nil)
	sink := make(strictOutcomeSink, 1)
	h.Gateway.Sink = sink
	body := fmt.Sprintf(`{"model":%q,"query":"q","documents":["private-marker"],"return_documents":true}`, route)
	status, output, _ := h.gatewayRaw("POST", "/native/voyage-rerank/models/"+route, key, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if status != 400 || !bytes.Contains(output, []byte(`"code":"content_policy_blocked"`)) || len(ranking.snapshot()) != 1 {
		t.Fatalf("blocked provider result became success or protocol failure: %d %s", status, output)
	}
	envelope := <-sink
	if len(envelope.Attempts) != 1 || envelope.Attempts[0].Class != "policy" || envelope.Attempts[0].Usage == nil || envelope.Attempts[0].Usage.TotalTokens != 3 || envelope.Attempts[0].BillingUncertain || envelope.Attempts[0].Interaction == nil {
		t.Fatalf("provider usage or local refusal lost: %+v", envelope)
	}
}

func TestStrictOperationInspectorUsesRegisteredContractWithoutDispatch(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	profiles := h.want(owner, "GET", "/api/v3/provider-profiles", nil, nil, 200)["items"].([]any)
	for id, operation := range map[string]string{
		"gemini-batch-embeddings": "embeddings", "openai-embeddings": "embeddings", "openai-input-tokens": "token_count", "openai-moderation": "moderation",
		"rerank": "rerank", "tei-classification": "classification", "tei-embeddings": "embeddings", "tei-multivector-embeddings": "embeddings",
		"tei-rerank": "rerank", "tei-scoring": "scoring", "tei-sparse-embeddings": "embeddings", "tei-tokenize": "token_count",
		"voyage-embeddings": "embeddings", "voyage-rerank": "rerank",
	} {
		found := false
		for _, raw := range profiles {
			profile := raw.(map[string]any)
			if profile["id"] != id {
				continue
			}
			found = true
			if profile["operation_dialects"].(map[string]any)[operation] != id || profile["default_schemas"].(map[string]any)[operation] == nil {
				t.Fatalf("deployed profile lacks its operation contract: %v", profile)
			}
		}
		if !found {
			t.Fatalf("deployed profile %s is unavailable", id)
		}
	}
	f := newOperationFixture(t, "/v1/embeddings", `{"data":[{"index":0,"embedding":[1,2]}],"usage":{"total_tokens":2}}`)
	slug, _ := publishOperation(t, h, owner, f, "voyage-embeddings", "embeddings", "native", nil)
	request := map[string]any{"model": slug, "input": "private-inspector-prompt", "output_dtype": "uint8", "encoding_format": "base64"}
	base := map[string]any{"operation": map[string]any{"operation": "embeddings", "route": slug, "request": request}, "surface": "native", "mode": "unary", "dialect": "voyage-embeddings", "seed": "registered-native-inspection"}
	inspect := func() map[string]any {
		t.Helper()
		rows := h.list(owner, "POST", "/api/v3/routing/simulate", base, nil, 200)
		if len(rows) != 1 {
			t.Fatalf("inspection target count: %v", rows)
		}
		return rows[0].(map[string]any)
	}
	refused := inspect()
	assertInspectorRejection(t, refused, "state_carrier")
	base["client_contract"] = "raw-vector-storage/1"
	admitted := inspect()
	interaction := admitted["interaction"].(map[string]any)
	if admitted["eligible"] != true || interaction["status"] != "admitted" || interaction["class"] != "native_identity" || interaction["operation"] != "embeddings" || interaction["ingress_dialect"] != "voyage-embeddings" || interaction["egress_dialect"] != "voyage-embeddings" || len(f.snapshot()) != 0 {
		t.Fatalf("registered inspection did not match dispatch semantics: %v", admitted)
	}
	encoded, _ := json.Marshal(admitted)
	if bytes.Contains(encoded, []byte("private-inspector-prompt")) || bytes.Contains(encoded, []byte(vendorSecret)) {
		t.Fatal("inspector exposed native input or provider credential")
	}
}
