package protocols

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func captureFromFrame(t *testing.T, family wire.Format, frame []byte) (texts []string, tools []string) {
	t.Helper()
	CaptureStreamFrame(family, frame, func(text string) { texts = append(texts, text) }, func(raw json.RawMessage) { tools = append(tools, string(raw)) })
	return texts, tools
}

func TestCaptureStreamFrameMistralAndCohere(t *testing.T) {
	texts, _ := captureFromFrame(t, wire.MistralFIM, []byte(
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"fim\"}}]}\n\n"))
	if len(texts) != 1 || texts[0] != "fim" {
		t.Fatalf("mistral texts %v", texts)
	}
	texts, tools := captureFromFrame(t, wire.CohereChat, []byte(
		"event: content-delta\ndata: {\"type\":\"content-delta\",\"delta\":{\"message\":{\"content\":{\"text\":\"coh\"}}}}\n\n"+
			"event: tool-call-start\ndata: {\"type\":\"tool-call-start\",\"delta\":{\"message\":{\"tool_calls\":{\"id\":\"tc1\",\"type\":\"function\",\"function\":{\"name\":\"g\"}}}}}\n\n"+
			"event: tool-call-delta\ndata: {\"type\":\"tool-call-delta\",\"delta\":{\"message\":{\"tool_calls\":{\"function\":{\"arguments\":\"{\\\"k\\\"\"}}}}}\n\n"))
	if len(texts) != 1 || texts[0] != "coh" {
		t.Fatalf("cohere texts %v", texts)
	}
	if len(tools) != 2 {
		t.Fatalf("cohere tools %v", tools)
	}
	if tools[0] != `{"id":"tc1","type":"function","function":{"name":"g"}}` {
		t.Fatalf("cohere tool start %s", tools[0])
	}
	if tools[1] != `{"function":{"arguments":"{\"k\""}}` {
		t.Fatalf("cohere tool delta %s", tools[1])
	}
}

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
