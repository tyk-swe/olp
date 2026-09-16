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
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// This is a deterministic real-management/real-gateway matrix. Cloud identity
// requests use local metadata and SigV4 fixtures, never paid provider accounts.
func TestProviderAndNativeSurfaceParity(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(home, ".config/gcloud/application_default_credentials.json")); err == nil {
		t.Fatal("run cloud fixture qualification without a personal gcloud ADC file")
	}
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			t.Error("missing Google metadata identity header")
		}
		w.Header().Set("Metadata-Flavor", "Google")
		switch r.URL.Path {
		case "/computeMetadata/v1/project/project-id":
			fmt.Fprint(w, "fixture-project")
		case "/computeMetadata/v1/instance/service-accounts/default/token":
			writeJSON(w, map[string]any{"access_token": "adc-fixture-token", "token_type": "Bearer", "expires_in": 3600})
		default:
			http.NotFound(w, r)
		}
	}))
	defer metadata.Close()
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadata.URL, "http://"))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	for _, kind := range []string{"openai", "openai_compatible", "anthropic", "gemini", "azure_openai", "vertex_ai", "bedrock"} {
		t.Run(kind, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			up := parityProvider(t, kind)
			cfg := map[string]any{"kind": kind, "auth_mode": "api_key", "endpoint": up.URL + "/v1"}
			credential := any(vendorSecret)
			switch kind {
			case "gemini":
				cfg["endpoint"] = up.URL + "/v1beta"
			case "azure_openai":
				cfg["endpoint"] = up.URL
				cfg["deployment"] = vendorModel
				cfg["api_version"] = "2024-10-21"
			case "vertex_ai":
				cfg["endpoint"] = up.URL + "/v1/projects/fixture-project/locations/us-central1/publishers/google"
				cfg["cloud_project"] = "fixture-project"
				cfg["cloud_region"] = "us-central1"
				cfg["auth_mode"] = "adc"
				credential = nil
			case "bedrock":
				cfg["endpoint"] = up.URL
				cfg["cloud_region"] = "us-east-1"
				cfg["auth_mode"] = "static"
				credential = `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789","session_token":"bedrock-session-token"}`
			}
			create := map[string]any{"name": "Parity " + kind, "configuration": cfg, "model": vendorModel}
			if credential != nil {
				create["credential"] = credential
			}
			detail := h.want(owner, "POST", "/api/v3/providers", create, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
			path := "/api/v3/providers/" + detail["id"].(string)
			probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(detail), 200)
			if probe["succeeded"] != true {
				t.Fatalf("native discovery/proof: %v", probe)
			}
			options := h.want(owner, "GET", "/api/v3/provider-kinds/"+kind+"/capabilities", nil, nil, 200)["capabilities"].([]any)
			capabilities := []any{}
			for _, option := range options {
				v := option.(map[string]any)
				capabilities = append(capabilities, map[string]any{"operation": v["operation"], "surface": v["surface"], "mode": v["mode"]})
			}
			models := h.want(owner, "GET", path+"/models", nil, nil, 200)
			modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
			detail = h.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": capabilities}, etagHeader(detail), 200)
			certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200)
			if certified["status"] != "certified" {
				t.Fatalf("full capability proof: %v", certified)
			}
			detail = h.want(owner, "GET", path, nil, nil, 200)
			h.want(owner, "POST", path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
			operations := []string{"generation", "token_count"}
			if kind == "openai" || kind == "openai_compatible" || kind == "azure_openai" {
				operations = append(operations, "embeddings", "moderation")
			}
			draft := h.want(owner, "POST", "/api/v3/route-drafts", map[string]any{"slug": routeSlug, "operations": operations, "overall_timeout_ms": 10000, "max_attempts": 1, "targets": []any{map[string]any{"provider_id": detail["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
			h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
			key := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "native inference", "scopes": []string{"inference", "models_read"}, "allowed_routes": []string{routeSlug}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
			read := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "models only", "scopes": []string{"models_read"}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
			h.refresh()
			for _, family := range []openai.Family{openai.FamilyChat, openai.FamilyResponses, openai.FamilyAnthropic, openai.FamilyGemini, openai.FamilyInputTokens, openai.FamilyAnthropicCount, openai.FamilyGeminiCount, openai.FamilyEmbeddings, openai.FamilyModeration} {
				for _, stream := range []bool{false, true} {
					if stream && family.Operation() != "generation" {
						continue
					}
					path, body, transport := parityRequest(family, stream)
					for _, version := range []string{"v1beta", "v1"} {
						if family.Surface() != "gemini" && version == "v1" {
							continue
						}
						clientPath := strings.ReplaceAll(path, "v1beta", version)
						status, _, _ := parityCall(t, h, read["secret"].(string), clientPath, body, family.Surface())
						if status != 403 {
							t.Fatalf("model-read key gained %s: %d", family.Operation(), status)
						}
						status, data, _ := parityCall(t, h, key["secret"].(string), clientPath, body, family.Surface())
						if !connectors.Supports(kind, "", family.Operation(), family.Surface(), map[bool]string{false: "unary", true: "streaming"}[stream]) {
							if status == 200 {
								t.Fatalf("uncertified %s/%s succeeded", kind, family)
							}
							continue
						}
						if status != 200 {
							t.Fatalf("%s stream=%v: %d %s", family, stream, status, data)
						}
						var completion *openai.Completion
						if stream {
							wire := transport
							if wire == openai.FamilyGeminiStream {
								wire = openai.FamilyGemini
							}
							completion, err = protocols.Stream(wire, transport, bytes.NewReader(data), 65536, routeSlug, true, func([]byte) error { return nil })
						} else {
							completion, err = protocols.Decode(transport, transport, data, routeSlug, "")
						}
						if err != nil {
							t.Fatalf("%s invalid native output: %v\n%s", family, err, data)
						}
						if family.Operation() != "moderation" && (completion.Usage == nil || completion.Usage.InputTokens < 1) {
							t.Fatalf("%s lost measured usage: %+v", family, completion)
						}
					}
				}
			}
			for _, prefix := range []string{"/v1", "/anthropic/v1", "/gemini/v1", "/gemini/v1beta"} {
				status, body, _ := h.gateway("GET", prefix+"/models/"+routeSlug, key["secret"].(string), nil)
				if status != 200 {
					t.Fatalf("gateway-owned model read %s: %d %v", prefix, status, body)
				}
			}
		})
	}
}

func parityRequest(family openai.Family, stream bool) (string, map[string]any, openai.Family) {
	body := map[string]any{"model": routeSlug}
	path := "/v1/chat/completions"
	transport := family
	switch family {
	case openai.FamilyChat:
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
		body["max_tokens"] = 16
		body["stream"] = stream
		if stream {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	case openai.FamilyResponses, openai.FamilyInputTokens:
		body["input"] = "hello"
		if family == openai.FamilyResponses {
			path = "/v1/responses"
			body["max_output_tokens"] = 16
			body["stream"] = stream
		} else {
			path = "/v1/responses/input_tokens"
		}
	case openai.FamilyEmbeddings:
		path = "/v1/embeddings"
		body["input"] = []string{"hello", "world"}
	case openai.FamilyModeration:
		path = "/v1/moderations"
		body["input"] = "hello"
	case openai.FamilyAnthropic, openai.FamilyAnthropicCount:
		path = "/anthropic/v1/messages"
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
		if family == openai.FamilyAnthropic {
			body["max_tokens"] = 16
			body["stream"] = stream
		} else {
			path += "/count_tokens"
		}
	case openai.FamilyGemini, openai.FamilyGeminiCount:
		body = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "hello"}}}}}
		action := "generateContent"
		if stream {
			action = "streamGenerateContent"
			transport = openai.FamilyGeminiStream
		}
		if family == openai.FamilyGeminiCount {
			action = "countTokens"
		} else {
			body["generationConfig"] = map[string]int{"maxOutputTokens": 16}
		}
		path = "/gemini/v1beta/models/" + routeSlug + ":" + action
	}
	return path, body, transport
}
func parityCall(t *testing.T, h *accessHarness, key, path string, body any, surface string) (int, []byte, http.Header) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(t.Context(), "POST", h.HTTP.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OLP-Routing", `{"strategy":"price","allow_fallbacks":false}`)
	switch surface {
	case "anthropic":
		req.Header.Set("X-Api-Key", key)
	case "gemini":
		req.Header.Set("X-Goog-Api-Key", key)
	default:
		req.Header.Set("Authorization", "Bearer "+key)
	}
	reply, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	data, err = io.ReadAll(reply.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply.StatusCode, data, reply.Header
}
func parityProvider(t *testing.T, kind string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-OLP-Routing") != "" {
			t.Error("raw routing preferences reached a provider")
		}
		auth := r.Header.Get("Authorization")
		switch kind {
		case "anthropic":
			if r.Header.Get("X-Api-Key") != vendorSecret || r.Header.Get("Anthropic-Version") == "" {
				t.Error("Anthropic authentication/version")
			}
		case "gemini":
			if r.Header.Get("X-Goog-Api-Key") != vendorSecret {
				t.Error("Gemini authentication")
			}
		case "azure_openai":
			if r.Header.Get("Api-Key") != vendorSecret || r.URL.Query().Get("api-version") != "2024-10-21" {
				t.Error("Azure authentication/version")
			}
		case "vertex_ai":
			if auth != "Bearer adc-fixture-token" {
				t.Error("Vertex ambient authentication")
			}
		case "bedrock":
			if !strings.Contains(auth, "Credential=BEDROCKKEY1234567890/") || r.Header.Get("X-Amz-Security-Token") != "bedrock-session-token" {
				t.Error("Bedrock SigV4 authentication")
			}
		default:
			if auth != "Bearer "+vendorSecret {
				t.Error("OpenAI authentication")
			}
		}
		if r.Method == "GET" {
			switch kind {
			case "gemini":
				writeJSON(w, map[string]any{"models": []any{map[string]string{"name": "models/" + vendorModel, "displayName": "Fixture"}}})
			case "bedrock":
				writeJSON(w, map[string]any{"modelSummaries": []any{map[string]string{"modelId": vendorModel, "modelName": "Fixture"}}})
			default:
				writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel, "display_name": "Fixture"}}})
			}
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "invalid body", 400)
			return
		}
		if model, ok := body["model"]; ok && model != vendorModel {
			t.Errorf("wire model %v", model)
		}
		path := r.URL.Path
		stream, _ := body["stream"].(bool)
		stream = stream || strings.HasSuffix(path, ":streamGenerateContent") || strings.HasSuffix(path, "/converse-stream")
		if strings.HasSuffix(path, "/input_tokens") || strings.HasSuffix(path, "/count_tokens") {
			writeJSON(w, map[string]int{"input_tokens": 13})
			return
		}
		if strings.HasSuffix(path, ":countTokens") {
			writeJSON(w, map[string]int{"totalTokens": 13})
			return
		}
		if strings.HasSuffix(path, "/count-tokens") {
			writeJSON(w, map[string]int{"inputTokens": 13})
			return
		}
		if strings.HasSuffix(path, "/embeddings") {
			writeJSON(w, map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float64{0.25, 0.5}}}, "usage": map[string]int{"prompt_tokens": 3, "total_tokens": 3}})
			return
		}
		if strings.HasSuffix(path, "/moderations") {
			writeJSON(w, map[string]any{"id": "mod-fixture", "model": vendorModel, "results": []any{map[string]any{"flagged": false, "categories": map[string]bool{"violence": false}, "category_scores": map[string]float64{"violence": 0}}}})
			return
		}
		if strings.HasSuffix(path, "/responses") {
			writeResponsesFixture(w, vendorModel, vendorAnswer, stream)
			return
		}
		parityGeneration(w, kind, stream)
	}))
	t.Cleanup(up.Close)
	return up
}
func parityGeneration(w http.ResponseWriter, kind string, stream bool) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	emit := func(name string, v any) {
		data, _ := json.Marshal(v)
		if name != "" {
			fmt.Fprintf(w, "event: %s\n", name)
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
	}
	switch kind {
	case "anthropic":
		message := map[string]any{"id": "msg-fixture", "type": "message", "role": "assistant", "model": vendorModel, "content": []any{map[string]string{"type": "text", "text": vendorAnswer}}, "stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]int{"input_tokens": 3, "output_tokens": 2}}
		if !stream {
			writeJSON(w, message)
			return
		}
		message["content"] = []any{}
		message["stop_reason"] = nil
		emit("message_start", map[string]any{"type": "message_start", "message": message})
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
		emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": vendorAnswer}})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 2}})
		emit("message_stop", map[string]string{"type": "message_stop"})
	case "gemini", "vertex_ai":
		response := map[string]any{"responseId": "gem-fixture", "modelVersion": vendorModel, "candidates": []any{map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": []any{map[string]string{"text": vendorAnswer}}}, "finishReason": "STOP"}}, "usageMetadata": map[string]int{"promptTokenCount": 3, "candidatesTokenCount": 2, "totalTokenCount": 5}}
		if stream {
			emit("", response)
		} else {
			writeJSON(w, response)
		}
	case "bedrock":
		if !stream {
			writeJSON(w, map[string]any{"output": map[string]any{"message": map[string]any{"role": "assistant", "content": []any{map[string]string{"text": vendorAnswer}}}}, "stopReason": "end_turn", "usage": map[string]int{"inputTokens": 3, "outputTokens": 2, "totalTokens": 5}})
			return
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		send := func(name string, v any) {
			headers := eventstream.Headers{}
			headers.Set(":message-type", eventstream.StringValue("event"))
			headers.Set(":event-type", eventstream.StringValue(name))
			payload, _ := json.Marshal(v)
			_ = eventstream.NewEncoder().Encode(w, eventstream.Message{Headers: headers, Payload: payload})
			w.(http.Flusher).Flush()
		}
		send("messageStart", map[string]string{"role": "assistant"})
		send("contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]string{"text": vendorAnswer}})
		send("contentBlockStop", map[string]int{"contentBlockIndex": 0})
		send("messageStop", map[string]string{"stopReason": "end_turn"})
		send("metadata", map[string]any{"usage": map[string]int{"inputTokens": 3, "outputTokens": 2, "totalTokens": 5}})
	default:
		if !stream {
			writeJSON(w, map[string]any{"id": "chat-fixture", "object": "chat.completion", "model": vendorModel, "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": vendorAnswer}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}})
			return
		}
		emit("", map[string]any{"id": "chat-fixture", "object": "chat.completion.chunk", "model": vendorModel, "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"role": "assistant", "content": vendorAnswer}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}})
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}
}
