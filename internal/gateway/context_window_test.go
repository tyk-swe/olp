package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func ptrInt64(v int64) *int64 { return &v }

func streamChunk(model, text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"`+model+`","choices":[{"index":0,"delta":{"content":"`+text+`"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}
}

func TestContextWindowClassificationIsExact(t *testing.T) {
	for code, want := range map[string]bool{
		"context_length_exceeded":     true,
		"Context-Length-Exceeded":     true,
		"context window exceeded":     true,
		"max_context_length_exceeded": true,
		"prompt_too_long":             true,
		"invalid_value":               false,
		"context_length":              false,
		"":                            false,
	} {
		if got := contextWindowError(&openai.UpstreamError{Code: code}); got != want {
			t.Errorf("code %q: got %v, want %v", code, got, want)
		}
	}
	if !contextWindowError(&openai.UpstreamError{Type: "context_window_exceeded"}) {
		t.Error("typed error type must classify")
	}
	if contextWindowError(&openai.UpstreamError{Message: "context_length_exceeded"}) {
		t.Error("message prose must never classify")
	}
	if contextWindowError(nil) {
		t.Error("nil upstream error must not classify")
	}
}

func TestContextWindowRejectionFailsOver(t *testing.T) {
	for _, code := range []string{"context_length_exceeded", "context_window_exceeded", "max_context_length_exceeded", "prompt_too_long"} {
		t.Run(code, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.mock.set("a", status(http.StatusBadRequest, `{"error":{"message":"too long","type":"invalid_request_error","code":"`+code+`"}}`))
			resp, body := h.chat(fullKey, nil)
			if resp.StatusCode != http.StatusOK || body["model"] != routeSlug {
				t.Fatalf("status %d body %v", resp.StatusCode, body)
			}
			env := h.sink.last(t)
			if len(env.Attempts) != 2 || env.Attempts[0].Class != classContextWindow || env.Attempts[0].Status != 400 || env.Attempts[1].Class != classSuccess {
				t.Fatalf("attempts %+v", env.Attempts)
			}
			if h.mock.count("a") != 1 || h.mock.count("b") != 1 {
				t.Fatalf("calls a=%d b=%d", h.mock.count("a"), h.mock.count("b"))
			}
		})
	}
}

func TestContextWindowRejectionExhaustedRendersUpstreamRejection(t *testing.T) {
	h := newHarness(t, Config{})
	for _, provider := range []string{"a", "b"} {
		h.mock.set(provider, status(http.StatusBadRequest, `{"error":{"message":"prompt too long","type":"invalid_request_error","code":"context_length_exceeded"}}`))
	}
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "upstream_rejected" {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	if message, _ := body["error"].(map[string]any)["message"].(string); message != "prompt too long" {
		t.Fatalf("upstream message dropped: %v", body)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 2 || env.Attempts[0].Class != classContextWindow || env.Attempts[1].Class != classContextWindow {
		t.Fatalf("attempts %+v", env.Attempts)
	}
}

func TestContextWindowInBandErrorFailsOverBeforeCommit(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"error":{"type":"invalid_request_error","code":"context_window_exceeded","message":"too long"}}`+"\n\n")
	})
	h.mock.set("b", streamChunk(modelB, answerText))
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), answerText) {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 2 || env.Attempts[0].Class != classContextWindow || env.Attempts[1].Class != classSuccess {
		t.Fatalf("attempts %+v", env.Attempts)
	}
}

func TestContextWindowInBandErrorAfterCommitCannotRestart(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"model-a","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
		io.WriteString(w, `data: {"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"too long"}}`+"\n\n")
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if resp.StatusCode != http.StatusOK || !strings.Contains(text, `"content":"partial"`) || !strings.Contains(text, `"code":"upstream_rejected"`) || strings.Contains(text, "[DONE]") {
		t.Fatalf("status %d body %q", resp.StatusCode, text)
	}
	env := h.sink.last(t)
	if env.Outcome != "failure" || !env.Committed || len(env.Attempts) != 1 || env.Attempts[0].Class != classContextWindow || !env.Attempts[0].Committed {
		t.Fatalf("envelope %+v", env)
	}
	if h.mock.count("b") != 0 {
		t.Fatal("a committed stream must never restart on another provider")
	}
}
