package protocols

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestCaptureDocumentToolCallsFamilies(t *testing.T) {
	for _, tc := range []struct {
		family wire.Format
		body   string
		want   int
	}{
		{wire.Chat, `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"hi","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`, 1},
		{wire.Responses, `{"output":[{"type":"message"},{"type":"function_call","id":"fc1","name":"g","arguments":"{}"}]}`, 1},
		{wire.Anthropic, `{"stop_reason":"tool_use","content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"s","input":{}}]}`, 1},
		{wire.Gemini, `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"thought":true,"text":"hidden"},{"text":"out"},{"functionCall":{"name":"l","args":{}}}]}}]}`, 1},
		{wire.Bedrock, `{"stopReason":"tool_use","output":{"message":{"content":[{"toolUse":{"toolUseId":"t","name":"n","input":{}}}]}}}`, 1},
		{wire.CohereChat, `{"finish_reason":"TOOL_CALL","message":{"tool_calls":[{"id":"t","type":"function","function":{"name":"g","arguments":"{}"}}]}}`, 1},
	} {
		calls := CaptureDocumentToolCalls(tc.family, []byte(tc.body))
		if len(calls) != tc.want {
			t.Fatalf("%s: %d tool calls, want %d", tc.family, len(calls), tc.want)
		}
	}
}

func TestCaptureUnaryRequiresCompleteNativeOutput(t *testing.T) {
	for _, tc := range []struct {
		family wire.Format
		body   string
		field  string
		good   string
		bad    string
	}{
		{wire.Chat, `{"choices":[{"message":{"content":"partial-sensitive-token"}}]}`, "finish_reason", "stop", "length"},
		{wire.MistralFIM, `{"choices":[{"message":{"content":"partial-sensitive-token"}}]}`, "finish_reason", "stop", "length"},
		{wire.Anthropic, `{"content":[{"type":"text","text":"partial-sensitive-token"}]}`, "stop_reason", "end_turn", "max_tokens"},
		{wire.Gemini, `{"candidates":[{"content":{"parts":[{"text":"partial-sensitive-token"}]}}]}`, "finishReason", "STOP", "MAX_TOKENS"},
		{wire.Bedrock, `{"output":{"message":{"content":[{"text":"partial-sensitive-token"}]}}}`, "stopReason", "end_turn", "max_tokens"},
		{wire.CohereChat, `{"message":{"content":[{"type":"text","text":"partial-sensitive-token"}]}}`, "finish_reason", "COMPLETE", "MAX_TOKENS"},
	} {
		t.Run(string(tc.family), func(t *testing.T) {
			for _, reason := range []any{tc.good, tc.bad, nil, 123, "unknown"} {
				var body map[string]any
				if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
					t.Fatal(err)
				}
				target := body
				if tc.family == wire.Chat || tc.family == wire.MistralFIM {
					target = body["choices"].([]any)[0].(map[string]any)
				} else if tc.family == wire.Gemini {
					target = body["candidates"].([]any)[0].(map[string]any)
				}
				if reason != nil {
					target[tc.field] = reason
				}
				raw, _ := json.Marshal(body)
				text, _, err := CaptureResponse(tc.family, raw, true, true)
				if reason == tc.good {
					if err != nil || text != "partial-sensitive-token" {
						t.Fatalf("complete output: %q %v", text, err)
					}
				} else if err == nil {
					t.Fatalf("accepted incomplete output with reason %v", reason)
				}
			}
		})
	}
}
