package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Registered generation dialects serve /native/{dialect}/models/{model}: the
// dialect lifts the caller body into the neutral source contract and strict
// interaction planning binds it. openai-chat is a legacy-path registration,
// so the same planning contract serves it end to end.
func TestNativeGenerationUnaryServesRegisteredDialect(t *testing.T) {
	h := strictHarness(t, nil)
	resp := h.do(t.Context(), http.MethodPost, "/native/openai-chat/models/"+routeSlug, fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}]}`), nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), answerText) || !strings.Contains(string(body), `"chat.completion"`) {
		t.Fatalf("native generation response malformed: %s", body)
	}
}

func TestNativeGenerationStreamingServesRegisteredDialect(t *testing.T) {
	h := strictHarness(t, nil)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"model-a\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\""+answerText+"\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
	})
	resp := h.do(t.Context(), http.MethodPost, "/native/openai-chat/models/"+routeSlug, fullKey, []byte(`{"model":"`+routeSlug+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`), nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type: got %q", ct)
	}
	if !strings.Contains(string(body), answerText) || !strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("native generation stream malformed: %s", body)
	}
}

func TestNativeGenerationRejectsUnregisteredDialect(t *testing.T) {
	h := strictHarness(t, nil)
	resp := h.do(t.Context(), http.MethodPost, "/native/not-a-dialect/models/"+routeSlug, fullKey, []byte(`{"model":"`+routeSlug+`","messages":[]}`), nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("unregistered dialect admitted: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "registered_dialect") && !strings.Contains(string(body), "target_capability") {
		t.Fatalf("unexpected error body: %s", body)
	}
}

func TestNativeGenerationRequiresStrictRoute(t *testing.T) {
	h := newHarness(t, Config{})
	resp := h.do(t.Context(), http.MethodPost, "/native/openai-chat/models/"+routeSlug, fullKey, []byte(`{"model":"`+routeSlug+`","messages":[]}`), nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("non-strict route admitted a native dialect: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "strict_native_operation") && !strings.Contains(string(body), "target_capability") {
		t.Fatalf("unexpected error body: %s", body)
	}
}

// A registered source dialect with no qualified mapping to the route target
// fails closed at planning; the request never reaches a provider.
func TestNativeGenerationRejectsUnmappedSourceDialect(t *testing.T) {
	h := strictHarness(t, nil)
	resp := h.do(t.Context(), http.MethodPost, "/native/anthropic-messages/models/"+routeSlug, fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`), nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("unmapped source dialect admitted: %d %s", resp.StatusCode, body)
	}
	if h.mock.count("a")+h.mock.count("b") != 0 {
		t.Fatal("unmapped source dialect reached a provider")
	}
}
