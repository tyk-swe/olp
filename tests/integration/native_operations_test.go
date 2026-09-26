//go:build integration

package integration_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

type nativeFixture struct {
	*httptest.Server
	lastPath atomic.Value
	lastBody atomic.Value
}

func (f *nativeFixture) record(r *http.Request, body map[string]any) {
	f.lastPath.Store(r.URL.Path)
	f.lastBody.Store(body)
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("upstream body: %v", err)
		return map[string]any{}
	}
	return body
}

func provisionRoute(t *testing.T, h *accessHarness, cfg map[string]any, credential any, model string, capabilities []any, operations []string) (string, string) {
	t.Helper()
	owner := h.owner()
	create := map[string]any{"name": "Native " + cfg["kind"].(string), "configuration": cfg, "model": model}
	if credential != nil {
		create["credential"] = credential
	}
	detail := h.want(owner, "POST", "/api/v1/providers", create, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	path := "/api/v1/providers/" + detail["id"].(string)
	probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(detail), 200)
	if probe["succeeded"] != true {
		t.Fatalf("provider probe: %v", probe)
	}
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	detail = h.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": capabilities}, etagHeader(detail), 200)
	certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200)
	if certified["status"] != "certified" {
		t.Fatalf("capability proof: %v", certified)
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	slug := "native-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"slug": slug, "operations": operations, "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": detail["id"], "provider_model": model, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "native inference", "scopes": []string{"inference", "models_read"}, "allowed_routes": []string{slug}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.refresh()
	return slug, key["secret"].(string)
}

func TestNativeEmbeddings(t *testing.T) {
	t.Run("gemini", func(t *testing.T) {
		fixture := &nativeFixture{}
		fixture.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				writeJSON(w, map[string]any{"models": []any{map[string]string{"name": "models/text-embedding-004", "displayName": "Embed"}}})
				return
			}
			if r.Header.Get("X-Goog-Api-Key") != vendorSecret {
				t.Error("Gemini authentication")
			}
			body := decodeBody(t, r)
			fixture.record(r, body)
			switch {
			case strings.HasSuffix(r.URL.Path, ":embedContent"):
				writeJSON(w, map[string]any{"embedding": map[string]any{"values": []float64{0.5, 0.6}}})
			case strings.HasSuffix(r.URL.Path, ":batchEmbedContents"):
				requests, _ := body["requests"].([]any)
				out := []any{}
				for range requests {
					out = append(out, map[string]any{"values": []float64{0.1, 0.2}})
				}
				writeJSON(w, map[string]any{"embeddings": out})
			default:
				http.Error(w, "unexpected "+r.URL.Path, 404)
			}
		}))
		t.Cleanup(fixture.Close)
		h := newAccessHarness(t)
		slug, secret := provisionRoute(t, h,
			map[string]any{"kind": "gemini", "auth_mode": "api_key", "endpoint": fixture.URL + "/v1beta"},
			vendorSecret, "text-embedding-004",
			[]any{map[string]any{"operation": "embeddings", "surface": "openai", "mode": "unary"}},
			[]string{"embeddings"})
		status, reply, _ := h.gateway("POST", "/v1/embeddings", secret, map[string]any{"model": slug, "input": "hello"})
		if status != 200 {
			t.Fatalf("single embed: %d %v", status, reply)
		}
		if path, _ := fixture.lastPath.Load().(string); !strings.HasSuffix(path, "/models/text-embedding-004:embedContent") {
			t.Fatalf("gemini single path: %s", path)
		}
		data, _ := reply["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("single embed data: %v", reply)
		}
		status, reply, _ = h.gateway("POST", "/v1/embeddings", secret, map[string]any{"model": slug, "input": []string{"a", "b"}})
		if status != 200 {
			t.Fatalf("batch embed: %d %v", status, reply)
		}
		if path, _ := fixture.lastPath.Load().(string); !strings.HasSuffix(path, ":batchEmbedContents") {
			t.Fatalf("gemini batch path: %s", path)
		}
		body, _ := fixture.lastBody.Load().(map[string]any)
		requests, _ := body["requests"].([]any)
		if len(requests) != 2 || requests[0].(map[string]any)["model"] != "models/text-embedding-004" {
			t.Fatalf("gemini batch body: %v", body)
		}
		if data, _ = reply["data"].([]any); len(data) != 2 {
			t.Fatalf("batch embed data: %v", reply)
		}
	})
	t.Run("vertex", func(t *testing.T) {
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
			if r.URL.Path == "/computeMetadata/v1/instance/service-accounts/default/token" {
				writeJSON(w, map[string]any{"access_token": "adc-fixture-token", "token_type": "Bearer", "expires_in": 3600})
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(metadata.Close)
		t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadata.URL, "http://"))
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
		fixture := &nativeFixture{}
		fixture.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer adc-fixture-token" {
				t.Error("Vertex ambient authentication")
			}
			body := decodeBody(t, r)
			fixture.record(r, body)
			switch {
			case strings.HasSuffix(r.URL.Path, ":countTokens"):
				writeJSON(w, map[string]any{"totalTokens": 5})
			case strings.HasSuffix(r.URL.Path, ":predict"):
				instances, _ := body["instances"].([]any)
				predictions := []any{}
				for i, instance := range instances {
					entry, _ := instance.(map[string]any)
					if entry["content"] == nil {
						t.Errorf("vertex instance missing content: %v", instance)
					}
					predictions = append(predictions, map[string]any{
						"embeddings": map[string]any{
							"values":     []float64{0.1 + float64(i)},
							"statistics": map[string]any{"token_count": i + 3},
						},
					})
				}
				writeJSON(w, map[string]any{"predictions": predictions})
			default:
				http.Error(w, "unexpected "+r.URL.Path, 404)
			}
		}))
		t.Cleanup(fixture.Close)
		h := newAccessHarness(t)
		slug, secret := provisionRoute(t, h,
			map[string]any{"kind": "vertex_ai", "auth_mode": "adc", "endpoint": fixture.URL + "/v1/projects/fixture-project/locations/us-central1/publishers/google", "cloud_project": "fixture-project", "cloud_region": "us-central1"},
			nil, "text-embedding-005",
			[]any{map[string]any{"operation": "embeddings", "surface": "openai", "mode": "unary"}},
			[]string{"embeddings"})
		status, reply, _ := h.gateway("POST", "/v1/embeddings", secret, map[string]any{"model": slug, "input": []string{"a", "b"}})
		if status != 200 {
			t.Fatalf("vertex embed: %d %v", status, reply)
		}
		if path, _ := fixture.lastPath.Load().(string); !strings.HasSuffix(path, "/models/text-embedding-005:predict") {
			t.Fatalf("vertex path: %s", path)
		}
		usage, _ := reply["usage"].(map[string]any)
		if usage["total_tokens"] != float64(7) {
			t.Fatalf("vertex usage: %v", reply)
		}
	})
	t.Run("bedrock", func(t *testing.T) {
		fixture := &nativeFixture{}
		fixture.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Authorization"), "Credential=BEDROCKKEY1234567890/") {
				t.Error("Bedrock SigV4 authentication")
			}
			if r.Method == "GET" {
				writeJSON(w, map[string]any{"modelSummaries": []any{map[string]string{"modelId": "amazon.titan-embed-text-v2:0", "modelName": "Titan Embed"}}})
				return
			}
			body := decodeBody(t, r)
			fixture.record(r, body)
			if body["inputText"] == nil {
				http.Error(w, "converse path not served", 404)
				return
			}
			writeJSON(w, map[string]any{"embedding": []float64{0.1, 0.2}, "inputTextTokenCount": 5})
		}))
		t.Cleanup(fixture.Close)
		h := newAccessHarness(t)
		slug, secret := provisionRoute(t, h,
			map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": fixture.URL, "cloud_region": "us-east-1"},
			`{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789","session_token":"bedrock-session-token"}`,
			"amazon.titan-embed-text-v2:0",
			[]any{map[string]any{"operation": "embeddings", "surface": "openai", "mode": "unary"}},
			[]string{"embeddings"})
		status, reply, _ := h.gateway("POST", "/v1/embeddings", secret, map[string]any{"model": slug, "input": "hello"})
		if status != 200 {
			t.Fatalf("bedrock embed: %d %v", status, reply)
		}
		if path, _ := fixture.lastPath.Load().(string); path != "/model/amazon.titan-embed-text-v2:0/invoke" {
			t.Fatalf("bedrock path: %s", path)
		}
		usage, _ := reply["usage"].(map[string]any)
		if usage["total_tokens"] != float64(5) {
			t.Fatalf("bedrock usage: %v", reply)
		}
	})
}

func TestRerank(t *testing.T) {
	for _, vendor := range []string{"voyage", "cohere"} {
		t.Run(vendor, func(t *testing.T) {
			fixture := &nativeFixture{}
			fixture.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+vendorSecret {
					t.Error("upstream authentication")
				}
				body := decodeBody(t, r)
				fixture.record(r, body)
				switch r.URL.Path {
				case "/v1/embeddings":
					writeJSON(w, map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float64{0.25}}}, "usage": map[string]int{"prompt_tokens": 3, "total_tokens": 3}})
				case "/v1/chat/completions":
					writeJSON(w, map[string]any{"id": "chatcmpl-1", "object": "chat.completion", "created": 1, "model": body["model"], "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 4, "completion_tokens": 6, "total_tokens": 10}})
				case "/v1/rerank":
					if vendor == "voyage" {
						if _, ok := body["top_n"]; ok {
							t.Errorf("voyage received canonical top_n: %v", body)
						}
						writeJSON(w, map[string]any{
							"data": []any{
								map[string]any{"index": 1, "relevance_score": 0.9, "document": "b"},
								map[string]any{"index": 0, "relevance_score": 0.4, "document": "a"},
							},
							"usage": map[string]int{"total_tokens": 9},
						})
						return
					}
					if _, ok := body["top_k"]; ok {
						t.Errorf("cohere received voyage top_k: %v", body)
					}
					writeJSON(w, map[string]any{
						"id": "rr-1",
						"results": []any{
							map[string]any{"index": 1, "relevance_score": 0.9, "document": "b"},
							map[string]any{"index": 0, "relevance_score": 0.4, "document": "a"},
						},
						"meta": map[string]any{"billed_units": map[string]any{"search_units": 1.5}},
					})
				default:
					http.Error(w, "unexpected "+r.URL.Path, 404)
				}
			}))
			t.Cleanup(fixture.Close)
			h := newAccessHarness(t)
			slug, secret := provisionRoute(t, h,
				map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": fixture.URL + "/v1", "options": map[string]any{"vendor_id": vendor}},
				vendorSecret, "rerank-2",
				[]any{map[string]any{"operation": "rerank", "surface": "openai", "mode": "unary"}},
				[]string{"rerank"})
			status, reply, _ := h.gateway("POST", "/v1/rerank", secret, map[string]any{"model": slug, "query": "q", "documents": []string{"a", "b"}, "top_n": 2, "return_documents": true})
			if status != 200 {
				t.Fatalf("rerank: %d %v", status, reply)
			}
			if path, _ := fixture.lastPath.Load().(string); path != "/v1/rerank" {
				t.Fatalf("rerank path: %s", path)
			}
			results, _ := reply["results"].([]any)
			if len(results) != 2 || results[0].(map[string]any)["index"] != float64(1) || results[0].(map[string]any)["relevance_score"] != 0.9 || results[0].(map[string]any)["document"] != "b" {
				t.Fatalf("rerank results: %v", reply)
			}
			if vendor == "voyage" {
				usage, _ := reply["usage"].(map[string]any)
				if usage["total_tokens"] != float64(9) {
					t.Fatalf("voyage usage: %v", reply)
				}
			}
			if vendor == "cohere" && reply["id"] != "rr-1" {
				t.Fatalf("cohere id: %v", reply)
			}
		})
	}
}

func TestNativeImage(t *testing.T) {
	pixel := base64.StdEncoding.EncodeToString([]byte{0x89, 0x50, 0x4e, 0x47})
	t.Run("vertex", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = os.Stat(filepath.Join(home, ".config/gcloud/application_default_credentials.json")); err == nil {
			t.Fatal("run cloud fixture qualification without a personal gcloud ADC file")
		}
		metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Metadata-Flavor", "Google")
			if r.URL.Path == "/computeMetadata/v1/instance/service-accounts/default/token" {
				writeJSON(w, map[string]any{"access_token": "adc-fixture-token", "token_type": "Bearer", "expires_in": 3600})
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(metadata.Close)
		t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadata.URL, "http://"))
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
		fixture := &nativeFixture{}
		fixture.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body := decodeBody(t, r)
			fixture.record(r, body)
			switch {
			case strings.HasSuffix(r.URL.Path, ":countTokens"):
				writeJSON(w, map[string]any{"totalTokens": 5})
			case strings.HasSuffix(r.URL.Path, ":predict"):
				parameters, _ := body["parameters"].(map[string]any)
				count := 1
				if n, ok := parameters["sampleCount"].(float64); ok {
					count = int(n)
				}
				predictions := []any{}
				for range count {
					predictions = append(predictions, map[string]any{"bytesBase64Encoded": pixel, "mimeType": "image/png"})
				}
				writeJSON(w, map[string]any{"predictions": predictions})
			default:
				http.Error(w, "unexpected "+r.URL.Path, 404)
			}
		}))
		t.Cleanup(fixture.Close)
		h := newAccessHarness(t)
		slug, secret := provisionRoute(t, h,
			map[string]any{"kind": "vertex_ai", "auth_mode": "adc", "endpoint": fixture.URL + "/v1/projects/fixture-project/locations/us-central1/publishers/google", "cloud_project": "fixture-project", "cloud_region": "us-central1"},
			nil, "imagen-3.0-generate-002",
			[]any{map[string]any{"operation": "image_generation", "surface": "openai", "mode": "unary"}},
			[]string{"image_generation"})
		status, reply, _ := h.gateway("POST", "/v1/images/generations", secret, map[string]any{"model": slug, "prompt": "a small illustration", "n": 2, "size": "1536x1024", "response_format": "b64_json"})
		if status != 200 {
			t.Fatalf("vertex image: %d %v", status, reply)
		}
		body, _ := fixture.lastBody.Load().(map[string]any)
		parameters, _ := body["parameters"].(map[string]any)
		if parameters["aspectRatio"] != "3:2" || parameters["sampleCount"] != float64(2) {
			t.Fatalf("vertex image body: %v", body)
		}
		data, _ := reply["data"].([]any)
		if len(data) != 2 || data[0].(map[string]any)["b64_json"] != pixel {
			t.Fatalf("vertex image reply: %v", reply)
		}
	})
	t.Run("bedrock", func(t *testing.T) {
		fixture := &nativeFixture{}
		fixture.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				writeJSON(w, map[string]any{"modelSummaries": []any{map[string]string{"modelId": "amazon.titan-image-generator-v2:0", "modelName": "Titan Image"}}})
				return
			}
			body := decodeBody(t, r)
			fixture.record(r, body)
			if body["taskType"] != "TEXT_IMAGE" {
				http.Error(w, "not an image task", 404)
				return
			}
			config, _ := body["imageGenerationConfig"].(map[string]any)
			count := 1
			if n, ok := config["numberOfImages"].(float64); ok {
				count = int(n)
			}
			images := []string{}
			for range count {
				images = append(images, pixel)
			}
			writeJSON(w, map[string]any{"images": images})
		}))
		t.Cleanup(fixture.Close)
		h := newAccessHarness(t)
		slug, secret := provisionRoute(t, h,
			map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": fixture.URL, "cloud_region": "us-east-1"},
			`{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789","session_token":"bedrock-session-token"}`,
			"amazon.titan-image-generator-v2:0",
			[]any{map[string]any{"operation": "image_generation", "surface": "openai", "mode": "unary"}},
			[]string{"image_generation"})
		status, reply, _ := h.gateway("POST", "/v1/images/generations", secret, map[string]any{"model": slug, "prompt": "a small illustration", "n": 2, "size": "768x768", "response_format": "b64_json"})
		if status != 200 {
			t.Fatalf("bedrock image: %d %v", status, reply)
		}
		if path, _ := fixture.lastPath.Load().(string); path != "/model/amazon.titan-image-generator-v2:0/invoke" {
			t.Fatalf("bedrock image path: %s", path)
		}
		body, _ := fixture.lastBody.Load().(map[string]any)
		config, _ := body["imageGenerationConfig"].(map[string]any)
		if config["width"] != float64(768) || config["numberOfImages"] != float64(2) {
			t.Fatalf("bedrock image body: %v", body)
		}
		data, _ := reply["data"].([]any)
		if len(data) != 2 || data[0].(map[string]any)["b64_json"] != pixel {
			t.Fatalf("bedrock image reply: %v", reply)
		}
	})
}
