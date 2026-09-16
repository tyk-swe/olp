//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Both OpenAI endpoints are independently certified for the published surface.
// Fixture responses therefore use the endpoint's own wire contract.
func writeResponsesFixture(w http.ResponseWriter, model, text string, stream bool) {
	response := map[string]any{"id": "resp_fixture", "object": "response", "created_at": 1, "model": model, "status": "completed", "error": nil, "incomplete_details": nil, "output": []any{map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}}, "usage": map[string]int{"input_tokens": 4, "output_tokens": 6, "total_tokens": 10}}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	delta, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": text})
	terminal, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
	_, _ = fmt.Fprintf(w, "event: response.output_text.delta\ndata: %s\n\nevent: response.completed\ndata: %s\n\n", delta, terminal)
	w.(http.Flusher).Flush()
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
