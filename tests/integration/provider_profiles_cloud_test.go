//go:build integration

package integration_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
)

// Every built-in composition reaches a real local provider through public
// configuration/certification/publication. Expected paths and bodies are authored
// here; no production encoder generates the reference side of these assertions.
func TestPublishedProviderProfilesPreserveCloudInvocation(t *testing.T) {
	testPublishedProviderProfilesPreserveCloudInvocation(t, false)
}

func TestStrictPublishedProviderProfilesPreserveCloudInvocation(t *testing.T) {
	testPublishedProviderProfilesPreserveCloudInvocation(t, true)
}

func testPublishedProviderProfilesPreserveCloudInvocation(t *testing.T, strict bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/gcloud/application_default_credentials.json")); err == nil {
		t.Fatal("fixture qualification requires no personal ADC file")
	}
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			t.Error("missing metadata identity header")
		}
		w.Header().Set("Metadata-Flavor", "Google")
		if strings.HasSuffix(r.URL.Path, "/token") {
			writeJSON(w, map[string]any{"access_token": "profile-adc-token", "token_type": "Bearer", "expires_in": 3600})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/project-id") {
			fmt.Fprint(w, "fixture-project")
			return
		}
		http.NotFound(w, r)
	}))
	defer metadata.Close()
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadata.URL, "http://"))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	// Pin the original generation catalogue independently of newly registered
	// unary-only profiles. The non-strict run also retains bedrock-invoke below.
	originalGeneration := []string{"openai-chat", "openai-responses", "compatible-chat", "compatible-responses", "anthropic-messages", "gemini-generation", "azure-legacy-chat", "azure-legacy-responses", "azure-v1-chat", "azure-v1-responses", "vertex-gemini", "vertex-anthropic", "bedrock-converse", "bedrock-anthropic-invoke"}
	seenOriginal := map[string]bool{}
	seenInvoke := false
	for _, profile := range connectors.Profiles() {
		if slices.Contains(originalGeneration, profile.ID) {
			seenOriginal[profile.ID] = true
			if !slices.Contains(profile.Operations, "generation") {
				t.Fatalf("original generation profile %s lost its operation", profile.ID)
			}
		}
		if profile.ID == "bedrock-invoke" {
			seenInvoke = true
		}
		// This fixture sends a generation request. Registered unary-only
		// profiles have their own public request/result suite in
		// strict_operations_test.go; retain the
		// existing Bedrock Invoke coverage in the non-strict run.
		if (!slices.Contains(profile.Operations, "generation") && profile.ID != "bedrock-invoke") || profile.ID == "gemini-interactions" {
			continue
		}
		if strict && profile.ID == "bedrock-invoke" {
			// The model-specific non-generation runner is qualified separately.
			continue
		}
		t.Run(profile.ID, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			model := vendorModel
			if profile.ID == "bedrock-invoke" {
				model = "amazon.titan-embed-text-v2:0"
			}
			type invocation struct {
				path    string
				query   string
				headers http.Header
				body    map[string]json.RawMessage
			}
			var mu sync.Mutex
			var calls []invocation
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					switch profile.Kind {
					case "gemini":
						writeJSON(w, map[string]any{"models": []any{map[string]string{"name": "models/" + model}}})
					case "bedrock":
						writeJSON(w, map[string]any{"modelSummaries": []any{map[string]string{"modelId": model}}})
					default:
						writeJSON(w, map[string]any{"data": []any{map[string]string{"id": model}}})
					}
					return
				}
				var body map[string]json.RawMessage
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					http.Error(w, "malformed JSON", 400)
					return
				}
				mu.Lock()
				calls = append(calls, invocation{r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body})
				mu.Unlock()
				stream := string(body["stream"]) == "true" || strings.HasSuffix(r.URL.Path, ":streamGenerateContent") || strings.HasSuffix(r.URL.Path, ":streamRawPredict") || strings.HasSuffix(r.URL.Path, "/converse-stream") || strings.HasSuffix(r.URL.Path, "/invoke-with-response-stream")
				if strings.HasSuffix(r.URL.Path, ":countTokens") {
					writeJSON(w, map[string]int{"totalTokens": 13})
					return
				}
				if profile.ID == "bedrock-invoke" {
					writeJSON(w, map[string]any{"embedding": []float64{0.25, 0.5}, "inputTextTokenCount": 13})
					return
				}
				if profile.Dialect == "openai-responses" {
					writeResponsesFixture(w, model, vendorAnswer, stream)
					return
				}
				if profile.ID == "bedrock-anthropic-invoke" && stream {
					w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
					native := httptest.NewRecorder()
					parityGeneration(native, "anthropic", true)
					for _, line := range strings.Split(native.Body.String(), "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						payload, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(strings.TrimPrefix(line, "data: ")))})
						headers := eventstream.Headers{}
						headers.Set(":message-type", eventstream.StringValue("event"))
						headers.Set(":event-type", eventstream.StringValue("chunk"))
						if err := eventstream.NewEncoder().Encode(w, eventstream.Message{Headers: headers, Payload: payload}); err != nil {
							t.Error(err)
						}
					}
					return
				}
				kind := profile.Kind
				if profile.Dialect == "anthropic-messages" {
					kind = "anthropic"
				}
				parityGeneration(w, kind, stream)
			}))
			defer upstream.Close()
			cfg := map[string]any{"kind": profile.Kind, "profile_id": profile.ID, "profile_revision": profile.Revision, "auth_mode": "api_key", "endpoint": upstream.URL + "/v1"}
			credential := any(vendorSecret)
			switch profile.Kind {
			case "gemini":
				cfg["endpoint"] = upstream.URL + "/v1beta"
			case "azure_openai":
				cfg["endpoint"] = upstream.URL
				cfg["deployment"] = model
				if profile.Hosting != "azure-v1" {
					cfg["api_version"] = "2025-04-01-preview"
				}
			case "vertex_ai":
				cfg["cloud_project"] = "fixture-project"
				cfg["cloud_region"] = "us-central1"
				cfg["auth_mode"] = "adc"
				publisher := "google"
				if profile.ID == "vertex-anthropic" {
					publisher = "anthropic"
				}
				cfg["endpoint"] = upstream.URL + "/v1/projects/fixture-project/locations/us-central1/publishers/" + publisher
				credential = nil
			case "bedrock":
				cfg["endpoint"] = upstream.URL
				cfg["cloud_region"] = "us-east-1"
				cfg["auth_mode"] = "static"
				credential = `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789","session_token":"bedrock-session-token"}`
			}
			input := map[string]any{"name": "Profile " + profile.ID, "configuration": cfg, "model": model}
			if credential != nil {
				input["credential"] = credential
			}
			created := h.want(owner, "POST", "/api/v1/providers", input, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
			providerPath := "/api/v1/providers/" + created["id"].(string)
			models := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)
			modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
			h.want(owner, "POST", providerPath+"/models/"+modelID+"/certify", nil, etagHeader(created), 200)
			current := h.want(owner, "GET", providerPath, nil, nil, 200)
			h.want(owner, "POST", providerPath+"/activate", nil, withMatch(current, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
			operation := "generation"
			if profile.ID == "bedrock-invoke" {
				operation = "bedrock_invoke"
			}
			routeInput := map[string]any{"slug": routeSlug, "operations": []string{operation}, "overall_timeout_ms": 10000, "max_attempts": 1, "targets": []any{map[string]any{"provider_id": created["id"], "provider_model": model, "priority": 0, "weight": 1, "timeout_ms": 5000}}}
			if strict {
				routeInput["fidelity"] = map[string]any{}
			}
			draft := h.want(owner, "POST", "/api/v1/route-drafts", routeInput, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
			h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
			key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Profile inference", "scopes": []string{"inference"}, "allowed_routes": []string{routeSlug}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
			h.refresh()
			path := "/v1/chat/completions"
			body := `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"capture"}],"max_tokens":32}`
			expectedPath := "/v1/chat/completions"
			switch profile.Dialect {
			case "openai-responses":
				path = "/v1/responses"
				body = `{"model":"` + routeSlug + `","input":[{"role":"user","content":[{"type":"input_text","text":"capture"}]}],"max_output_tokens":32,"store":false}`
				expectedPath = "/v1/responses"
			case "anthropic-messages":
				path = "/anthropic/v1/messages"
				body = `{"model":"` + routeSlug + `","messages":[{"role":"user","content":[{"type":"text","text":"capture"}]}],"max_tokens":32}`
				expectedPath = "/v1/messages"
			case "gemini-generate-content":
				path = "/gemini/v1beta/models/" + routeSlug + ":generateContent"
				body = `{"contents":[{"role":"user","parts":[{"text":"capture"}]}],"generationConfig":{"maxOutputTokens":32}}`
				expectedPath = "/v1beta/models/" + model + ":generateContent"
			case "bedrock-converse":
				path = "/bedrock/model/" + routeSlug + "/converse"
				body = `{"messages":[{"role":"user","content":[{"text":"capture"}]}],"inferenceConfig":{"maxTokens":32}}`
				expectedPath = "/model/" + model + "/converse"
			case "bedrock-invoke":
				path = "/bedrock/model/" + routeSlug + "/invoke"
				body = `{"inputText":"capture","dimensions":2}`
				expectedPath = "/model/" + model + "/invoke"
			}
			switch profile.Hosting {
			case "azure-v1":
				expectedPath = "/openai" + expectedPath
			case "azure-deployment":
				expectedPath = "/openai/deployments/" + model + strings.TrimPrefix(expectedPath, "/v1")
			case "azure-responses-legacy":
				expectedPath = "/openai/responses"
			case "vertex-google":
				expectedPath = "/v1/projects/fixture-project/locations/us-central1/publishers/google/models/" + model + ":generateContent"
			case "vertex-anthropic":
				expectedPath = "/v1/projects/fixture-project/locations/us-central1/publishers/anthropic/models/" + model + ":rawPredict"
			case "bedrock-anthropic-invoke":
				expectedPath = "/model/" + model + "/invoke"
			}
			headers := map[string]string{"Content-Type": "application/json"}
			if strict && profile.Dialect == "anthropic-messages" {
				headers["Anthropic-Version"] = "2023-06-01"
			}
			status, result, _ := h.gatewayRaw("POST", path, key["secret"].(string), strings.NewReader(body), headers)
			if status != 200 {
				t.Fatalf("published invocation: %d %s", status, result)
			}
			if profile.ID != "bedrock-invoke" && !bytes.Contains(result, []byte(vendorAnswer)) {
				t.Fatalf("native response lost output: %s", result)
			}
			mu.Lock()
			actual := calls[len(calls)-1]
			mu.Unlock()
			if actual.path != expectedPath {
				t.Fatalf("captured endpoint=%s want=%s", actual.path, expectedPath)
			}
			if strict {
				// Independently construct only the contract's model/hosting identity
				// changes. All input structure, controls and presence must be equal.
				var expected map[string]json.RawMessage
				if err := json.Unmarshal([]byte(body), &expected); err != nil {
					t.Fatal(err)
				}
				if _, exists := expected["model"]; exists {
					expected["model"], _ = json.Marshal(model)
				}
				if profile.Hosting == "vertex-anthropic" || profile.Hosting == "bedrock-anthropic-invoke" {
					delete(expected, "model")
					expected["anthropic_version"], _ = json.Marshal(profile.DialectRevision)
				}
				want, _ := json.Marshal(expected)
				got, _ := json.Marshal(actual.body)
				requireProfileNetworkJSON(t, string(want), got)
			}
			if profile.Kind == "azure_openai" {
				if actual.headers.Get("Api-Key") != vendorSecret {
					t.Fatal("Azure authentication lost")
				}
				if profile.Hosting == "azure-v1" && actual.query != "" {
					t.Fatal("Azure v1 acquired dated query")
				}
				if profile.Hosting != "azure-v1" && actual.query != "api-version=2025-04-01-preview" {
					t.Fatal("Azure API version lost")
				}
			}
			if profile.Kind == "vertex_ai" && actual.headers.Get("Authorization") != "Bearer profile-adc-token" {
				t.Fatal("Vertex access token lost")
			}
			if profile.Kind == "bedrock" && (!strings.Contains(actual.headers.Get("Authorization"), "/us-east-1/bedrock/aws4_request") || actual.headers.Get("X-Amz-Security-Token") != "bedrock-session-token") {
				t.Fatal("completed Bedrock invocation was not signed")
			}
			if profile.Hosting == "vertex-anthropic" || profile.Hosting == "bedrock-anthropic-invoke" {
				if strict && actual.headers.Get("Anthropic-Version") != "" {
					t.Fatal("cloud API version was sent in an unsupported header instead of its native body binding")
				}
				if _, found := actual.body["model"]; found {
					t.Fatal("cloud model left in body")
				}
				if string(actual.body["anthropic_version"]) != `"`+profile.DialectRevision+`"` {
					t.Fatal("cloud body revision lost")
				}
				if !bytes.Contains(actual.body["messages"], []byte(`"text":"capture"`)) {
					t.Fatal("cloud wrapper changed input")
				}
			}
			if profile.Dialect == "openai-responses" {
				if _, found := actual.body["messages"]; found {
					t.Fatal("Responses profile fell through Chat")
				}
				if !bytes.Contains(actual.body["input"], []byte(`"text":"capture"`)) {
					t.Fatal("Responses input changed")
				}
			}
			if strict && profile.Dialect == "anthropic-messages" {
				mu.Lock()
				before := len(calls)
				mu.Unlock()
				headers["Anthropic-Version"] = "2099-01-01"
				status, reply, _ := h.gatewayRaw("POST", path, key["secret"].(string), strings.NewReader(body), headers)
				mu.Lock()
				after := len(calls)
				mu.Unlock()
				if status != 400 || !bytes.Contains(reply, []byte(`"code":"target_capability"`)) || before != after {
					t.Fatalf("unknown ingress API revision dispatched: %d %s", status, reply)
				}
			}
		})
	}
	for _, id := range originalGeneration {
		if !seenOriginal[id] {
			t.Fatalf("original generation profile %s disappeared", id)
		}
	}
	if !seenInvoke {
		t.Fatal("original Bedrock Invoke profile disappeared")
	}
}
