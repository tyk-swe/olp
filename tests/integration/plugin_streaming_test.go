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

const streamedAnswer = "Hello from the streaming-only upstream"

// streamingUpstream is the fictional upstream the reference plugin's
// reference-streaming profile places requests at: OpenAI Responses under
// /streaming/v1 that serves only streaming requests. It refuses, and counts,
// any request that does not ask for a stream, and it delivers its output item
// only in response.output_item.done, leaving the terminal response's output
// empty.
type streamingUpstream struct {
	*httptest.Server
	refused atomic.Int64
}

func newStreamingUpstream(t *testing.T) *streamingUpstream {
	t.Helper()
	u := &streamingUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token "+pluginCredential || r.Header.Get("X-Reference-Client") != "olp" {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]any{"error": map[string]any{"message": "Unknown token", "type": "invalid_request_error", "code": "invalid_api_key"}})
			return
		}
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/streaming/v1/responses" || json.NewDecoder(r.Body).Decode(&body) != nil || !body.Stream {
			u.refused.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": map[string]any{"message": "Stream must be set to true", "type": "invalid_request_error"}})
			return
		}
		response := func(status string) map[string]any {
			return map[string]any{"id": "resp_streaming", "object": "response", "created_at": 1, "status": status, "model": body.Model, "output": []any{}}
		}
		completed := response("completed")
		completed["usage"] = map[string]any{"input_tokens": 5, "output_tokens": 7, "total_tokens": 12}
		message := func(status string, content ...any) map[string]any {
			return map[string]any{"id": "msg_streaming", "type": "message", "role": "assistant", "status": status, "content": content}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i, event := range []map[string]any{
			{"type": "response.created", "response": response("in_progress")},
			{"type": "response.output_item.added", "output_index": 0, "item": message("in_progress")},
			{"type": "response.output_text.delta", "item_id": "msg_streaming", "output_index": 0, "content_index": 0, "delta": streamedAnswer},
			{"type": "response.output_item.done", "output_index": 0, "item": message("completed", map[string]any{"type": "output_text", "text": streamedAnswer, "annotations": []any{}})},
			{"type": "response.completed", "response": completed},
		} {
			event["sequence_number"] = i
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

// The upstream refuses anything but a stream, so certifying the unary
// capability proves that certification aggregates the stream too.

// The aggregated request and the streaming one leave the same usage and
// pricing records.
