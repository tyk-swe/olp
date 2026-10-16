//go:build integration

package integration_test

import (
	"io"
	"net/http"
	"net/http/httptest"
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
	responseType string
	expectedPath string
	policy       any
}

func newOperationFixture(t *testing.T, path, response string) *operationFixture {
	t.Helper()
	f := &operationFixture{response: response, responseType: "application/json", expectedPath: path}
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
		contentType := f.responseType
		f.mu.Unlock()
		if r.URL.Path != path {
			t.Errorf("native path %s, want %s", r.URL.Path, path)
		}
		w.Header().Set("Content-Type", contentType)
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
func (f *operationFixture) resultType(value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responseType = value
}
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
	provider := h.want(owner, "POST", "/api/v1/providers", providerInput, idem(uuid.NewString()), 201)
	providerPath := "/api/v1/providers/" + provider["id"].(string)
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
	draft := h.want(owner, "POST", "/api/v1/route-drafts", draftInput, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Native operation", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem(uuid.NewString()), 201)
	h.refresh()
	return slug, key["secret"].(string)
}

func operationCapabilities(profile, operation, surface string) []any {
	out := []any{map[string]any{"operation": operation, "surface": surface, "mode": "unary"}}
	if surface == "native" && (profile == "voyage-embeddings" || profile == "voyage-rerank" || profile == "rerank") {
		out = append(out, map[string]any{"operation": operation, "surface": "openai", "mode": "unary"})
	}
	return out
}

// A native operation is estimated from the size of its document, whatever
// model it is sent to, and says so.
