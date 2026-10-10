package protocols

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func captureFromFrame(t *testing.T, family openai.Family, frame []byte) (texts []string, tools []string) {
	t.Helper()
	CaptureStreamFrame(family, frame, func(text string) { texts = append(texts, text) }, func(raw json.RawMessage) { tools = append(tools, string(raw)) })
	return texts, tools
}

func TestCaptureStreamFrameChat(t *testing.T) {
	texts, tools := captureFromFrame(t, openai.FamilyChat, []byte(
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hel\"}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"refusal\":\"no\"}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]}}]}\n\n"+
			"data: [DONE]\n\n"))
	wantTools := []string{
		`{"index":0,"id":"call_1","function":{"name":"f","arguments":"{\"a\":"}}`,
		`{"index":0,"function":{"arguments":"1}"}}`,
	}
	if strings.Join(texts, "|") != "hel|no" {
		t.Fatalf("texts %v", texts)
	}
	if strings.Join(tools, "|") != strings.Join(wantTools, "|") {
		t.Fatalf("tools %v", tools)
	}
}

func TestCaptureStreamFrameResponses(t *testing.T) {
	texts, tools := captureFromFrame(t, openai.FamilyResponses, []byte(
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"+
			"event: response.refusal.delta\ndata: {\"type\":\"response.refusal.delta\",\"delta\":\"nope\"}\n\n"+
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"tool\",\"call_id\":\"c1\"}}\n\n"+
			"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"{\\\"x\\\"\"}\n\n"+
			"event: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"arguments\":\"{\\\"x\\\":1}\"}\n\n"+
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"tool\",\"call_id\":\"c1\",\"arguments\":\"{\\\"x\\\":1}\"}}\n\n"))
	if strings.Join(texts, "|") != "hello|nope" {
		t.Fatalf("texts %v", texts)
	}
	wantTools := []string{
		`{"id":"fc_1","type":"function_call","name":"tool","call_id":"c1"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"x\""}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"x\":1}"}`,
		`{"id":"fc_1","type":"function_call","name":"tool","call_id":"c1","arguments":"{\"x\":1}"}`,
	}
	if strings.Join(tools, "|") != strings.Join(wantTools, "|") {
		t.Fatalf("tools %v", tools)
	}
}

func TestCaptureStreamFrameAnthropic(t *testing.T) {
	texts, tools := captureFromFrame(t, openai.FamilyAnthropic, []byte(
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"+
			"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu_1\",\"name\":\"search\"}}\n\n"+
			"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\"\"}}\n\n"))
	if len(texts) != 1 || texts[0] != "hi" {
		t.Fatalf("texts %v", texts)
	}
	if len(tools) != 2 {
		t.Fatalf("tools %v", tools)
	}
	if tools[0] != `{"type":"tool_use","id":"tu_1","name":"search"}` {
		t.Fatalf("tool_use start %s", tools[0])
	}
	if tools[1] != `{"type":"input_json_delta","partial_json":"{\"q\""}` {
		t.Fatalf("input_json_delta %s", tools[1])
	}
}

func TestCaptureStreamFrameGeminiExcludesThought(t *testing.T) {
	texts, tools := captureFromFrame(t, openai.FamilyGemini, []byte(
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"thought\":true,\"text\":\"secret chain\"},{\"text\":\"visible\"},{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"x\":1}}}]}}]}\n\n"))
	if len(texts) != 1 || texts[0] != "visible" {
		t.Fatalf("thought content captured: %v", texts)
	}
	if len(tools) != 1 || tools[0] != `{"name":"lookup","args":{"x":1}}` {
		t.Fatalf("functionCall %v", tools)
	}
}

func TestCaptureStreamFrameMistralAndCohere(t *testing.T) {
	texts, _ := captureFromFrame(t, openai.FamilyMistralFIM, []byte(
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"fim\"}}]}\n\n"))
	if len(texts) != 1 || texts[0] != "fim" {
		t.Fatalf("mistral texts %v", texts)
	}
	texts, tools := captureFromFrame(t, openai.FamilyCohereChat, []byte(
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

func TestCaptureStreamFrameBedrockNative(t *testing.T) {
	var frame bytes.Buffer
	payload := `{"contentBlockIndex":0,"delta":{"text":"bed"}}`
	headers := eventstream.Headers{{Name: ":message-type", Value: eventstream.StringValue("event")}, {Name: ":event-type", Value: eventstream.StringValue("contentBlockDelta")}}
	if err := eventstream.NewEncoder().Encode(&frame, eventstream.Message{Headers: headers, Payload: []byte(payload)}); err != nil {
		t.Fatal(err)
	}
	texts, tools := captureFromFrame(t, openai.FamilyBedrock, frame.Bytes())
	if len(texts) != 1 || texts[0] != "bed" || len(tools) != 0 {
		t.Fatalf("bedrock text %v tools %v", texts, tools)
	}
	frame.Reset()
	payload = `{"contentBlockIndex":0,"delta":{"toolUse":{"input":"{\"q\""}}}`
	if err := eventstream.NewEncoder().Encode(&frame, eventstream.Message{Headers: headers, Payload: []byte(payload)}); err != nil {
		t.Fatal(err)
	}
	texts, tools = captureFromFrame(t, openai.FamilyBedrock, frame.Bytes())
	if len(tools) != 1 || len(texts) != 0 || tools[0] != `{"toolUse":{"input":"{\"q\""}}` {
		t.Fatalf("bedrock toolUse delta texts %v tools %v", texts, tools)
	}
	frame.Reset()
	payload = `{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"tu1","name":"lookup"}}}`
	headers = eventstream.Headers{{Name: ":message-type", Value: eventstream.StringValue("event")}, {Name: ":event-type", Value: eventstream.StringValue("contentBlockStart")}}
	if err := eventstream.NewEncoder().Encode(&frame, eventstream.Message{Headers: headers, Payload: []byte(payload)}); err != nil {
		t.Fatal(err)
	}
	_, tools = captureFromFrame(t, openai.FamilyBedrock, frame.Bytes())
	if len(tools) != 1 || tools[0] != `{"toolUseId":"tu1","name":"lookup"}` {
		t.Fatalf("bedrock toolUse start %v", tools)
	}
}

func TestCaptureDocumentToolCallsMistral(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"ok","tool_calls":[{"id":"m1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}}]}`)
	calls := CaptureDocumentToolCalls(openai.FamilyMistralFIM, body)
	if len(calls) != 1 || string(calls[0]) != `{"id":"m1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}` {
		t.Fatalf("mistral tool calls %v", calls)
	}
}

func TestCaptureDocumentToolCallsFamilies(t *testing.T) {
	for _, tc := range []struct {
		family openai.Family
		body   string
		want   int
	}{
		{openai.FamilyChat, `{"choices":[{"message":{"role":"assistant","content":"hi","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`, 1},
		{openai.FamilyResponses, `{"output":[{"type":"message"},{"type":"function_call","id":"fc1","name":"g","arguments":"{}"}]}`, 1},
		{openai.FamilyAnthropic, `{"content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"s","input":{}}]}`, 1},
		{openai.FamilyGemini, `{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"hidden"},{"text":"out"},{"functionCall":{"name":"l","args":{}}}]}}]}`, 1},
		{openai.FamilyBedrock, `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"t","name":"n","input":{}}}]}}}`, 1},
		{openai.FamilyCohereChat, `{"message":{"tool_calls":[{"id":"t","type":"function","function":{"name":"g","arguments":"{}"}}]}}`, 1},
	} {
		calls := CaptureDocumentToolCalls(tc.family, []byte(tc.body))
		if len(calls) != tc.want {
			t.Fatalf("%s: %d tool calls, want %d", tc.family, len(calls), tc.want)
		}
	}
}
