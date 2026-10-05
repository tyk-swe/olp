//go:build integration

package integration_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// provisionStrictNative activates a provider in a native generation dialect,
// certified for that dialect's unary and streaming tuples, behind a strict
// route.
func provisionStrictNative(t *testing.T, h *accessHarness, cfg map[string]any, model string) (string, string) {
	t.Helper()
	owner := h.owner()
	detail := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Native " + cfg["profile_id"].(string), "configuration": cfg, "model": model, "credential": vendorSecret}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	path := "/api/v1/providers/" + detail["id"].(string)
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)
	var modelID string
	for _, item := range models["items"].([]any) {
		if row := item.(map[string]any); row["upstream_model"] == model {
			modelID = row["id"].(string)
		}
	}
	if modelID == "" {
		t.Fatalf("model %s was not declared: %v", model, models)
	}
	native := []any{map[string]any{"operation": "generation", "surface": "native", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "native", "mode": "streaming"}}
	detail = h.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": native}, etagHeader(detail), 200)
	certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200)
	if certified["status"] != "certified" {
		t.Fatalf("native certification: %v", certified)
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	slug := "native-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"slug": slug, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{}, "targets": []any{map[string]any{"provider_id": detail["id"], "provider_model": model, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "native generation", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.refresh()
	return slug, key["secret"].(string)
}

func nativeStream(t *testing.T, h *accessHarness, path, secret, body string) (int, string) {
	t.Helper()
	status, reply, _ := h.gatewayRaw("POST", path, secret, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
	return status, string(reply)
}

func TestMistralFillInTheMiddleIsServedNatively(t *testing.T) {
	fixture := &nativeFixture{}
	fixture.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": "codestral-2508"}}})
			return
		}
		if r.URL.Path != "/v1/fim/completions" || r.Header.Get("Authorization") != "Bearer "+vendorSecret {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		body := decodeBody(t, r)
		fixture.record(r, body)
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, chunk := range []string{
				`{"id":"fim-1","object":"chat.completion.chunk","created":1759496862,"model":"codestral-2508","choices":[{"index":0,"delta":{"role":"assistant","content":"a + b"},"finish_reason":null}]}`,
				`{"id":"fim-1","object":"chat.completion.chunk","created":1759496862,"model":"codestral-2508","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":3,"total_tokens":11}}`,
				`[DONE]`,
			} {
				io.WriteString(w, "data: "+chunk+"\n\n")
			}
			return
		}
		writeJSON(w, map[string]any{"id": "fim-1", "object": "chat.completion", "model": "codestral-2508", "created": 1759496862,
			"usage":   map[string]int{"prompt_tokens": 8, "completion_tokens": 3, "total_tokens": 11},
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "a + b", "tool_calls": nil, "prefix": false}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(fixture.Close)
	h := newAccessHarness(t)
	slug, secret := provisionStrictNative(t, h, map[string]any{"kind": "openai_compatible", "profile_id": "mistral-fim", "profile_revision": "1", "auth_mode": "api_key", "endpoint": fixture.URL + "/v1", "options": map[string]any{"vendor_id": "mistral"}}, "codestral-2508")
	path := "/native/mistral-fim/models/" + slug
	status, reply, _ := h.gateway("POST", path, secret, map[string]any{"prompt": "def add(a, b):\n    return ", "suffix": "\n", "max_tokens": 32})
	if status != http.StatusOK || reply["model"] != slug {
		t.Fatalf("unary FIM: %d %v", status, reply)
	}
	if body, _ := fixture.lastBody.Load().(map[string]any); body["model"] != "codestral-2508" || body["prompt"] != "def add(a, b):\n    return " || body["suffix"] != "\n" || len(body) != 4 {
		t.Fatalf("upstream FIM request: %v", body)
	}
	status, streamed := nativeStream(t, h, path, secret, `{"prompt":"def add(a, b):","stream":true}`)
	if status != http.StatusOK || !strings.Contains(streamed, `"content":"a + b"`) || !strings.Contains(streamed, `"model":"`+slug+`"`) || strings.Contains(streamed, "codestral-2508") || !strings.HasSuffix(streamed, "data: [DONE]\n\n") {
		t.Fatalf("streamed FIM: %d %s", status, streamed)
	}
	for name, invalid := range map[string]map[string]any{
		"no prompt":     {"suffix": "x"},
		"another model": {"model": "other-route", "prompt": "x"},
	} {
		if status, reply, _ := h.gateway("POST", path, secret, invalid); status != http.StatusBadRequest {
			t.Fatalf("%s: %d %v", name, status, reply)
		}
	}
	// The route serves its native dialect only: a chat request finds no
	// provider that speaks Chat Completions.
	if status, reply, _ := h.gateway("POST", "/v1/chat/completions", secret, map[string]any{"model": slug, "messages": []any{map[string]string{"role": "user", "content": "hi"}}}); status == http.StatusOK {
		t.Fatalf("a chat request reached the FIM target: %v", reply)
	}
}

func TestCohereChatIsServedNatively(t *testing.T) {
	fixture := &nativeFixture{}
	fixture.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+vendorSecret {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		body := decodeBody(t, r)
		switch r.URL.Path {
		case "/v2/embed":
			writeJSON(w, map[string]any{"id": "e", "embeddings": map[string]any{"float": []any{[]float64{0.1, 0.2}}}, "texts": body["texts"], "meta": map[string]any{"billed_units": map[string]int{"input_tokens": 2}}})
		case "/v2/chat":
			fixture.record(r, body)
			if body["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				stream := bufio.NewWriter(w)
				for _, event := range []string{
					`message-start`, `{"delta":{"message":{"role":"assistant"}},"id":"29f14a5a","type":"message-start"}`,
					`content-start`, `{"delta":{"message":{"content":{"text":"","type":"text"}}},"index":0,"type":"content-start"}`,
					`content-delta`, `{"delta":{"message":{"content":{"text":"Hello"}}},"index":0,"type":"content-delta"}`,
					`citation-start`, `{"delta":{"message":{"citations":{"end":5,"sources":[{"document":{"id":"doc-1","snippet":"Hello"},"id":"doc-1","type":"document"}],"start":0,"text":"Hello","type":"TEXT_CONTENT"}}},"index":0,"type":"citation-start"}`,
					`citation-end`, `{"index":0,"type":"citation-end"}`,
					`content-end`, `{"index":0,"type":"content-end"}`,
					`message-end`, `{"delta":{"finish_reason":"COMPLETE","usage":{"billed_units":{"input_tokens":5,"output_tokens":1},"tokens":{"input_tokens":71,"output_tokens":1}}},"type":"message-end"}`,
				} {
					if strings.HasPrefix(event, "{") {
						io.WriteString(stream, "data: "+event+"\n\n")
					} else {
						io.WriteString(stream, "event: "+event+"\n")
					}
				}
				stream.Flush()
				return
			}
			writeJSON(w, map[string]any{"id": "c1", "finish_reason": "COMPLETE",
				"message": map[string]any{"role": "assistant", "content": []any{map[string]string{"type": "text", "text": "Hello"}},
					"citations": []any{map[string]any{"start": 0, "end": 5, "text": "Hello", "sources": []any{map[string]any{"type": "document", "id": "doc-1", "document": map[string]string{"id": "doc-1", "snippet": "Hello"}}}}}},
				"usage": map[string]any{"billed_units": map[string]int{"input_tokens": 5, "output_tokens": 1}, "tokens": map[string]int{"input_tokens": 71, "output_tokens": 1}}})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(fixture.Close)
	h := newAccessHarness(t)
	slug, secret := provisionStrictNative(t, h, map[string]any{"kind": "openai_compatible", "profile_id": "cohere-v2", "profile_revision": "1", "auth_mode": "api_key", "endpoint": fixture.URL + "/v2", "options": map[string]any{"vendor_id": "cohere-native-v2"}}, "command-a-03-2025")
	path := "/native/cohere-chat-v2/models/" + slug
	request := map[string]any{"messages": []any{map[string]string{"role": "user", "content": "Say hello."}},
		"documents": []any{map[string]any{"id": "doc-1", "data": map[string]string{"snippet": "Hello"}}}, "citation_options": map[string]string{"mode": "ACCURATE"}}
	status, reply, _ := h.gateway("POST", path, secret, request)
	citations, _ := reply["message"].(map[string]any)["citations"].([]any)
	if status != http.StatusOK || len(citations) != 1 || reply["finish_reason"] != "COMPLETE" {
		t.Fatalf("unary Cohere chat: %d %v", status, reply)
	}
	if body, _ := fixture.lastBody.Load().(map[string]any); body["model"] != "command-a-03-2025" || body["documents"] == nil || body["citation_options"] == nil {
		t.Fatalf("upstream Cohere request: %v", body)
	}
	request["stream"] = true
	encoded, _ := json.Marshal(request)
	status, streamed := nativeStream(t, h, path, secret, string(encoded))
	if status != http.StatusOK || !strings.Contains(streamed, "event:citation-start\n") || !strings.Contains(streamed, `"text":"Hello"`) || !strings.Contains(streamed, "event:message-end\n") {
		t.Fatalf("streamed Cohere chat: %d %s", status, streamed)
	}
}
