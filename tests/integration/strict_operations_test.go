//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	provider := h.want(owner, "POST", "/api/v3/providers", map[string]any{"name": "Operation " + uuid.NewString(), "configuration": configuration, "model": vendorModel, "credential": vendorSecret}, idem(uuid.NewString()), 201)
	providerPath := "/api/v3/providers/" + provider["id"].(string)
	modelID := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)["items"].([]any)[0].(map[string]any)["id"].(string)
	provider = h.want(owner, "PATCH", providerPath+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": []any{map[string]any{"operation": operation, "surface": surface, "mode": "unary"}}}, etagHeader(provider), 200)
	f.mu.Lock()
	originalResult := f.response
	f.mu.Unlock()
	switch profileID {
	case "voyage-embeddings":
		f.result(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1,2]}],"model":"fixture-model","usage":{"total_tokens":2}}`)
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
	h.want(owner, "POST", providerPath+"/models/"+modelID+"/certify", nil, etagHeader(provider), 200)
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
	for _, test := range []struct{ profile, path, request, result string }{{"tei-sparse-embeddings", "embed_sparse", `{"inputs":["a","b"],"truncate":null,"prompt_name":null}`, `[[{"index":900,"value":-0},{"index":2,"value":1.0000001}],[{"index":1000,"value":0.2}]]`}, {"tei-multivector-embeddings", "embed_all", `{"inputs":["a","b"],"truncate":false,"truncation_direction":"Left"}`, `[[[0.1,0.2],[0.3,0.4]],[[0.5,0.6]]]`}} {
		t.Run(test.profile, func(t *testing.T) {
			f := newOperationFixture(t, "/v1/"+test.path, test.result)
			slug, key := publishOperation(t, h, owner, f, test.profile, "embeddings", "native", nil)
			status, body, _ := h.gatewayRaw("POST", "/native/"+test.profile+"/models/"+slug, key, strings.NewReader(test.request), map[string]string{"Content-Type": "application/json", "X-OLP-Client-Contract": "raw-vector-storage/1"})
			if status != 200 || string(body) != test.result {
				t.Fatalf("native storage: %d %s", status, body)
			}
			if len(f.snapshot()) != 1 || string(f.snapshot()[0].body) != test.request {
				t.Fatal("native source was normalized")
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
