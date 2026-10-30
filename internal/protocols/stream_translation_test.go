package protocols

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestChatGeminiMultipleCandidatesSurviveUnaryAndStreaming(t *testing.T) {
	source := []byte(`{"id":"multi","choices":[{"index":0,"message":{"role":"assistant","content":"alpha"},"finish_reason":"stop"},{"index":1,"message":{"role":"assistant","content":"beta"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}`)
	c, err := Decode(wire.Chat, wire.Gemini, source, "team-model", "")
	if err != nil {
		t.Fatal(err)
	}
	f, _ := object(c.Body)
	if len(arr(f["candidates"])) != 2 || !bytes.Contains(c.Body, []byte("beta")) {
		t.Fatalf("candidates %s", c.Body)
	}
	if _, err = Decode(wire.Chat, wire.Anthropic, source, "team-model", ""); err == nil {
		t.Fatal("silently dropped a candidate")
	}
	streamed := "data: " + `{"id":"multi","choices":[{"index":0,"delta":{"content":"alpha"},"finish_reason":"stop"},{"index":1,"delta":{"content":"beta"},"finish_reason":"stop"}]}` + "\n\ndata: " + `{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}` + "\n\ndata: [DONE]\n\n"
	var gem bytes.Buffer
	_, err = Stream(wire.Chat, wire.Gemini, strings.NewReader(streamed), 8192, "team-model", true, func(b []byte) error { gem.Write(b); return nil })
	if err != nil {
		t.Fatal(err)
	}
	var chat bytes.Buffer
	summary, err := Stream(wire.Gemini, wire.Chat, bytes.NewReader(gem.Bytes()), 8192, "team-model", true, func(b []byte) error { chat.Write(b); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if summary.Usage == nil || summary.Usage.TotalTokens != 4 || strings.Count(chat.String(), "[DONE]") != 1 || !strings.Contains(chat.String(), `"index":1`) || !strings.Contains(chat.String(), `"role":"assistant"`) {
		t.Fatalf("lost candidate/usage/completion: %s", chat.String())
	}
	if _, err = Stream(wire.Chat, wire.Chat, bytes.NewReader(chat.Bytes()), 8192, "team-model", true, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestCountingKeepsBedrockToolsAndInvalidGeneratedArgumentsRefuseTranslation(t *testing.T) {
	request, err := Parse(wire.InputTokens, []byte(`{"model":"team-model","input":"hello","tools":[{"type":"function","name":"weather","parameters":{"type":"object","properties":{}}}]}`), "team-model")
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := Encode(request, "bedrock", "amazon-bedrock", "wire-model", nil)
	if err != nil || !bytes.Contains(body, []byte("toolConfig")) || bytes.Contains(body, []byte("inferenceConfig")) {
		t.Fatalf("count input lost tool tokens: %s %v", body, err)
	}
	malformed := []byte(`{"id":"t","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call","type":"function","function":{"name":"weather","arguments":"not-json"}}]},"finish_reason":"tool_calls"}]}`)
	if _, err = Decode(wire.Chat, wire.Anthropic, malformed, "team-model", ""); err == nil {
		t.Fatal("fabricated a native tool input from malformed generated arguments")
	}
}

func TestGeminiParallelResultsKeepDistinctCallRelationships(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"weather","args":{"city":"Paris"}}},{"functionCall":{"name":"weather","args":{"city":"London"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"weather","response":{"temperature":20}}},{"functionResponse":{"name":"weather","response":{"temperature":16}}}]}]}`)
	request, err := Parse(wire.Gemini, input, "team-model")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, err := Encode(request, "openai", "openai", "wire-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	document, _ := object(encoded)
	messages := arr(document["messages"])
	assistant, _ := object(messages[0])
	calls := arr(assistant["tool_calls"])
	if len(messages) != 3 || len(calls) != 2 {
		t.Fatalf("lost parallel tools: %s", encoded)
	}
	for i, call := range calls {
		tool, _ := object(call)
		result, _ := object(messages[i+1])
		if str(tool["id"]) != str(result["tool_call_id"]) {
			t.Fatalf("tool result attached to another call: %s", encoded)
		}
	}
	for _, mime := range []string{"", "text/plain"} {
		bad := []byte(`{"contents":[{"parts":[{"text":"hello"}]}],"generationConfig":{"responseMimeType":"` + mime + `","responseSchema":{"type":"object"}}}`)
		request, err = Parse(wire.Gemini, bad, "team-model")
		if err != nil {
			continue
		}
		if _, _, err = Encode(request, "openai", "openai", "wire-model", nil); err == nil {
			t.Fatal("response schema was silently dropped")
		}
	}
}

func TestResponsesToolSnapshotsSurviveStreamingAndAggregation(t *testing.T) {
	for _, fragmented := range []bool{false, true} {
		t.Run(fmt.Sprintf("fragmented=%t", fragmented), func(t *testing.T) {
			created := `{"type":"response.created","response":{"id":"tools","status":"in_progress","output":[]}}`
			item := `{"type":"function_call","call_id":"call-read","name":"read_file","arguments":"{\"path\":\"main.go\"}"}`
			terminal := `{"type":"response.completed","response":{"id":"tools","status":"completed","output":[` + item + `],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`
			deltas := ""
			if fragmented {
				deltas = "data: " + `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call-read","name":"read_file","arguments":""}}` + "\n\ndata: " + `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":"}` + "\n\ndata: " + `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"main.go\"}"}` + "\n\ndata: " + `{"type":"response.output_item.done","output_index":0,"item":` + item + `}` + "\n\n"
			}
			source := "data: " + created + "\n\n" + deltas + "data: " + terminal + "\n\n"
			unary, err := AggregateTranslatedResponses(wire.Anthropic, strings.NewReader(source), 8192, "route", nil)
			if err != nil {
				t.Fatal(err)
			}
			if unary.FinishReason != "tool_calls" || len(unary.ToolCalls) != 1 || unary.ToolCalls[0].ID != "call-read" || unary.ToolCalls[0].Name != "read_file" || unary.ToolCalls[0].Arguments != `{"path":"main.go"}` || unary.Usage == nil || unary.Usage.TotalTokens != 5 || !unary.UsageComplete {
				t.Fatalf("aggregated tools %+v", unary)
			}
			var frames bytes.Buffer
			if _, err := Stream(wire.Responses, wire.Anthropic, strings.NewReader(source), 8192, "route", true, func(frame []byte) error { frames.Write(frame); return nil }); err != nil {
				t.Fatal(err)
			}
			capture := NewCaptureStream(wire.Anthropic, 8192, true, true)
			if err := capture.Push(frames.Bytes()); err != nil {
				t.Fatal(err)
			}
			_, calls, err := capture.Finish()
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) != 1 {
				t.Fatalf("stream tool count %d", len(calls))
			}
			var streamed struct {
				Name      string
				Arguments json.RawMessage
			}
			if err := json.Unmarshal(calls[0], &streamed); err != nil {
				t.Fatal(err)
			}
			if streamed.Name != unary.ToolCalls[0].Name || string(streamed.Arguments) != unary.ToolCalls[0].Arguments || bytes.Count(frames.Bytes(), []byte(`"id":"call-read"`)) != 1 || !bytes.Contains(frames.Bytes(), []byte(`"stop_reason":"tool_use"`)) || !bytes.Contains(frames.Bytes(), []byte(`"output_tokens":2`)) {
				t.Fatalf("stream tools %s, aggregate %+v", frames.String(), unary)
			}

		})
	}
}

func TestTranslatedResponsesRefuseOpaqueReasoning(t *testing.T) {
	body := `{"id":"opaque","status":"completed","output":[{"type":"reasoning","encrypted_content":"opaque-native-state"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	if _, err := Decode(wire.Responses, wire.Anthropic, []byte(body), "route", ""); err == nil {
		t.Fatal("opaque reasoning silently disappeared")
	}
	stream := "data: " + `{"type":"response.completed","response":` + body + `}` + "\n\n"
	if _, err := AggregateTranslatedResponses(wire.Anthropic, strings.NewReader(stream), 8192, "route", nil); err == nil {
		t.Fatal("aggregated opaque reasoning silently disappeared")
	}
}
