//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// carriedCredential is the static credential of the providers the carrier
// plugin serves.
const carriedCredential = "sk-carried"

// carriedUpstream answers only chat requests the carrier plugin carried,
// which it marks. A stream whose prompt is "hold" pauses after its first
// event until the test releases it.
type carriedUpstream struct {
	*httptest.Server
	carried, refused atomic.Int64
	release          chan struct{}
}

func newCarriedUpstream(t *testing.T) *carriedUpstream {
	t.Helper()
	u := &carriedUpstream{release: make(chan struct{})}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Carried-By") != "carrier" || r.Header.Get("Authorization") != "Bearer "+carriedCredential {
			u.refused.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]any{"error": map[string]any{"message": "The plugin did not carry this request.", "type": "invalid_request_error", "code": "invalid_api_key"}})
			return
		}
		u.carried.Add(1)
		var body struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		usage := map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
		if !body.Stream {
			writeJSON(w, map[string]any{"id": "chatcmpl-carried", "object": "chat.completion", "created": 1, "model": body.Model,
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "Hello from the carried upstream"}, "finish_reason": "stop"}}, "usage": usage})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(chunk map[string]any) {
			chunk["id"], chunk["object"], chunk["created"], chunk["model"] = "chatcmpl-carried", "chat.completion.chunk", 1, body.Model
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			w.(http.Flusher).Flush()
		}
		event(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "Hello"}, "finish_reason": nil}}})
		if len(body.Messages) > 0 && body.Messages[0].Content == "hold" {
			select {
			case <-u.release:
			case <-r.Context().Done():
				return
			}
		}
		event(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": " again"}, "finish_reason": nil}}})
		event(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usage})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(u.Close)
	return u
}

// The upstream answers only what the plugin carries, so control's probe
// and certification travel through it too.

// The upstream holds the rest of the stream until the caller has read
// its first event.

// Default SDK retries must not repeat work whose result the carrier lost,
// for either a unary request or a stream that failed before its first event.
