package protocols

import (
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestCaptureDocumentToolCallsFamilies(t *testing.T) {
	for _, tc := range []struct {
		family wire.Format
		body   string
		want   int
	}{
		{wire.Chat, `{"choices":[{"message":{"role":"assistant","content":"hi","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`, 1},
		{wire.Responses, `{"output":[{"type":"message"},{"type":"function_call","id":"fc1","name":"g","arguments":"{}"}]}`, 1},
		{wire.Anthropic, `{"content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"s","input":{}}]}`, 1},
		{wire.Gemini, `{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"hidden"},{"text":"out"},{"functionCall":{"name":"l","args":{}}}]}}]}`, 1},
		{wire.Bedrock, `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"t","name":"n","input":{}}}]}}}`, 1},
		{wire.CohereChat, `{"message":{"tool_calls":[{"id":"t","type":"function","function":{"name":"g","arguments":"{}"}}]}}`, 1},
	} {
		calls := CaptureDocumentToolCalls(tc.family, []byte(tc.body))
		if len(calls) != tc.want {
			t.Fatalf("%s: %d tool calls, want %d", tc.family, len(calls), tc.want)
		}
	}
}
