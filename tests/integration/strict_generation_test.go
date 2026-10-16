//go:build integration

package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway"
)

type strictProviderCall struct {
	body    []byte
	headers http.Header
	query   url.Values
}

type strictProviderFixture struct {
	*httptest.Server
	mu          sync.Mutex
	calls       []strictProviderCall
	profile     string
	providerID  string
	failure     atomic.Int32
	extraOutput atomic.Bool
}

func newStrictProviderFixture(t *testing.T, profile string) *strictProviderFixture {
	t.Helper()
	f := &strictProviderFixture{profile: profile}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if profile == "gemini-generation" {
				writeJSON(w, map[string]any{"models": []any{map[string]string{"name": "models/" + vendorModel}}})
			} else {
				writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			}
			return
		}
		var raw []byte
		var err error
		if f.failure.Load() == 3 {
			raw = make([]byte, 1)
			_, err = io.ReadFull(r.Body, raw)
		} else {
			raw, err = io.ReadAll(r.Body)
		}
		if err != nil {
			t.Error(err)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, strictProviderCall{raw, r.Header.Clone(), r.URL.Query()})
		f.mu.Unlock()
		if failure := f.failure.Load(); failure != 0 {
			if failure == 2 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"id":`)
				_ = http.NewResponseController(w).Flush()
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
			return
		}
		stream := string(body["stream"]) == "true" || strings.HasSuffix(r.URL.Path, ":streamGenerateContent")
		nativeWriter := w
		var buffered *httptest.ResponseRecorder
		if f.extraOutput.Load() && !stream {
			buffered = httptest.NewRecorder()
			nativeWriter = buffered
		}
		switch profile {
		case "compatible-responses", "azure-v1-responses":
			writeResponsesFixture(nativeWriter, vendorModel, vendorAnswer, stream)
		case "anthropic-messages":
			kindGeneration(nativeWriter, "anthropic", stream)
		case "gemini-generation":
			kindGeneration(nativeWriter, "gemini", stream)
		default:
			kindGeneration(nativeWriter, "openai", stream)
		}
		if buffered != nil {
			var result map[string]json.RawMessage
			if err := json.Unmarshal(buffered.Body.Bytes(), &result); err != nil {
				t.Error(err)
				return
			}
			result["native_extension"] = json.RawMessage(`{"private_text":"private-marker"}`)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(result)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *strictProviderFixture) captured() []strictProviderCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]strictProviderCall(nil), f.calls...)
}

func publishStrictProvider(t *testing.T, h *accessHarness, owner *browser, f *strictProviderFixture, options map[string]any, policy map[string]any, mode string) (string, string) {
	t.Helper()
	profile, err := connectors.LookupProfile(f.profile, "1")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := f.URL + "/v1"
	if f.profile == "gemini-generation" {
		endpoint = f.URL + "/v1beta"
	}
	if profile.Kind == "azure_openai" {
		endpoint = f.URL
	}
	config := map[string]any{"kind": profile.Kind, "profile_id": profile.ID, "profile_revision": profile.Revision, "endpoint": endpoint, "auth_mode": "api_key"}
	if profile.Kind == "azure_openai" {
		config["deployment"] = vendorModel
	}
	if options != nil {
		config["options"] = options
	}
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Strict " + f.profile + uuid.NewString(), "configuration": config, "model": vendorModel, "credential": vendorSecret}, idem(uuid.NewString()), 201)
	providerPath := "/api/v1/providers/" + provider["id"].(string)
	f.providerID = provider["id"].(string)
	modelID := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)["items"].([]any)[0].(map[string]any)["id"].(string)
	surfaces := []string{"openai"}
	if profile.Dialect == "anthropic-messages" {
		surfaces = append(surfaces, "anthropic")
	}
	if profile.Dialect == "gemini-generate-content" {
		surfaces = append(surfaces, "gemini")
	}
	capabilities := []any{}
	for _, surface := range surfaces {
		for _, delivery := range []string{"unary", "streaming"} {
			capabilities = append(capabilities, map[string]any{"operation": "generation", "surface": surface, "mode": delivery})
		}
	}
	provider = h.want(owner, "PATCH", providerPath+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": capabilities}, etagHeader(provider), 200)
	h.want(owner, "POST", providerPath+"/models/"+modelID+"/certify", nil, etagHeader(provider), 200)
	provider = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(provider, idem(uuid.NewString())), 200)
	slug := "strict-" + uuid.NewString()
	input := fidelityDraft(slug, provider["id"])
	input["fidelity"] = map[string]any{"mode": mode}
	if policy != nil {
		input["content_policy"] = policy
	}
	draft := h.want(owner, "POST", "/api/v1/route-drafts", input, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Strict fixture", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem(uuid.NewString()), 201)
	h.refresh()
	return slug, key["secret"].(string)
}

type strictOutcomeSink chan gateway.Envelope

func (s strictOutcomeSink) Terminal(e gateway.Envelope) { s <- e }

// fixed opaque PDF fixture bytes
