package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A successful SDK receipt only means something if the scripted provider
// actually rejects lost/reordered native dependencies and changed controls.
func TestNativeToolWorkflowRejectsCorruptedNextRequests(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing signature", func(body map[string]any) {
			blocks := body["messages"].([]any)[1].(map[string]any)["content"].([]any)
			delete(blocks[0].(map[string]any), "signature")
		}},
		{"text moved before tools", func(body map[string]any) {
			blocks := body["messages"].([]any)[1].(map[string]any)["content"].([]any)
			blocks[2], blocks[4] = blocks[4], blocks[2]
		}},
		{"changed tool identity", func(body map[string]any) {
			results := body["messages"].([]any)[2].(map[string]any)["content"].([]any)
			results[1].(map[string]any)["tool_use_id"] = "call-weather"
		}},
		{"changed reasoning budget", func(body map[string]any) {
			body["thinking"].(map[string]any)["budget_tokens"] = 512
		}},
		{"missing tool description", func(body map[string]any) {
			delete(body["tools"].([]any)[0].(map[string]any), "description")
		}},
		{"missing history", func(body map[string]any) {
			body["messages"] = body["messages"].([]any)[1:]
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflow, err := newNativeToolWorkflow()
			if err != nil {
				t.Fatal(err)
			}
			send := func(body []byte) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/native-tools/v1/messages", bytes.NewReader(body))
				r.Header.Set("X-Api-Key", credential)
				r.Header.Set("Anthropic-Version", "2023-06-01")
				w := httptest.NewRecorder()
				workflow.ServeHTTP(w, r)
				return w
			}
			if initial := send(workflow.initial); initial.Code != http.StatusOK || !bytes.Equal(initial.Body.Bytes(), workflow.events) {
				t.Fatal("native workflow did not emit the unchanged frozen stream")
			}
			var next map[string]any
			if err := json.Unmarshal(workflow.next, &next); err != nil {
				t.Fatal(err)
			}
			test.change(next)
			body, err := json.Marshal(next)
			if err != nil {
				t.Fatal(err)
			}
			if reply := send(body); reply.Code != http.StatusBadRequest {
				t.Fatal("scripted provider accepted a corrupted native next request")
			}
			if got := workflow.snapshot(); got != (nativeToolCounts{Dispatches: 2, InitialRequests: 1, Rejected: 1}) {
				t.Fatalf("corrupted continuation incorrectly qualified: %+v", got)
			}
		})
	}
}
