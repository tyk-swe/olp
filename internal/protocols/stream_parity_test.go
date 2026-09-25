package protocols

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/sse"
)

func TestTranslatedToolIdentityArgumentsAndCompletionSequence(t *testing.T) {
	source := `data: {"id":"tool-stream","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_weather","type":"function","function":{"name":"wea","arguments":""}}]},"finish_reason":null}]}

data: {"id":"tool-stream","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"ther","arguments":"{\"city\":\""}}]},"finish_reason":null}]}

data: {"id":"tool-stream","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"Paris\"}"}}]},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}

data: [DONE]

`
	for _, target := range []openai.Family{openai.FamilyAnthropic, openai.FamilyGemini, openai.FamilyResponses} {
		t.Run(string(target), func(t *testing.T) {
			var out bytes.Buffer
			c, err := Stream(openai.FamilyChat, target, strings.NewReader(source), 8192, "team-model", true, func(frame []byte) error { out.Write(frame); return nil })
			if err != nil {
				t.Fatal(err)
			}
			if c.Usage == nil || c.Usage.TotalTokens != 5 {
				t.Fatalf("usage %#v", c.Usage)
			}
			if !strings.Contains(out.String(), "weather") {
				t.Fatalf("fragmented tool name was lost: %s", out.String())
			}
			if _, err = Stream(target, target, bytes.NewReader(out.Bytes()), 8192, "team-model", true, func([]byte) error { return nil }); err != nil {
				t.Fatalf("invalid destination: %v\n%s", err, out.String())
			}
			if target == openai.FamilyResponses {
				last := -1
				events := map[string]bool{}
				var terminal Object
				err = sse.Decode(bytes.NewReader(out.Bytes()), 8192, func(event sse.Frame) error {
					f, _ := object([]byte(event.Data))
					seq, _ := count(f["sequence_number"])
					if int(seq) <= last {
						t.Fatalf("sequence %d after %d", seq, last)
					}
					last = int(seq)
					events[str(f["type"])] = true
					if str(f["type"]) == "response.completed" {
						terminal, _ = object(f["response"])
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.output_item.done", "response.completed"} {
					if !events[name] {
						t.Fatalf("missing %s", name)
					}
				}
				items := arr(terminal["output"])
				item, _ := object(items[0])
				if str(item["name"]) != "weather" || str(item["arguments"]) != `{"city":"Paris"}` {
					t.Fatalf("terminal tool: %s", raw(item))
				}
			}
		})
	}
}

func TestChatGeminiMultipleCandidatesSurviveUnaryAndStreaming(t *testing.T) {
	source := []byte(`{"id":"multi","choices":[{"index":0,"message":{"role":"assistant","content":"alpha"},"finish_reason":"stop"},{"index":1,"message":{"role":"assistant","content":"beta"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}`)
	c, err := Decode(openai.FamilyChat, openai.FamilyGemini, source, "team-model", "")
	if err != nil {
		t.Fatal(err)
	}
	f, _ := object(c.Body)
	if len(arr(f["candidates"])) != 2 || !bytes.Contains(c.Body, []byte("beta")) {
		t.Fatalf("candidates %s", c.Body)
	}
	if _, err = Decode(openai.FamilyChat, openai.FamilyAnthropic, source, "team-model", ""); err == nil {
		t.Fatal("silently dropped a candidate")
	}
	streamed := "data: " + `{"id":"multi","choices":[{"index":0,"delta":{"content":"alpha"},"finish_reason":"stop"},{"index":1,"delta":{"content":"beta"},"finish_reason":"stop"}]}` + "\n\ndata: " + `{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}` + "\n\ndata: [DONE]\n\n"
	var gem bytes.Buffer
	_, err = Stream(openai.FamilyChat, openai.FamilyGemini, strings.NewReader(streamed), 8192, "team-model", true, func(b []byte) error { gem.Write(b); return nil })
	if err != nil {
		t.Fatal(err)
	}
	var chat bytes.Buffer
	summary, err := Stream(openai.FamilyGemini, openai.FamilyChat, bytes.NewReader(gem.Bytes()), 8192, "team-model", true, func(b []byte) error { chat.Write(b); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if summary.Usage == nil || summary.Usage.TotalTokens != 4 || strings.Count(chat.String(), "[DONE]") != 1 || !strings.Contains(chat.String(), `"index":1`) || !strings.Contains(chat.String(), `"role":"assistant"`) {
		t.Fatalf("lost candidate/usage/completion: %s", chat.String())
	}
	if _, err = Stream(openai.FamilyChat, openai.FamilyChat, bytes.NewReader(chat.Bytes()), 8192, "team-model", true, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestRefusalAndEmbeddingEncodingArePreserved(t *testing.T) {
	refused := []byte(`{"id":"r","choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":"Cannot answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	for _, target := range []openai.Family{openai.FamilyResponses, openai.FamilyAnthropic, openai.FamilyGemini} {
		c, err := Decode(openai.FamilyChat, target, refused, "team-model", "")
		if err != nil || !bytes.Contains(c.Body, []byte("Cannot answer")) {
			t.Fatalf("%s refusal: %#v %v", target, c, err)
		}
	}
	floats := []byte(`{"data":[{"index":0,"embedding":[0.5,-1.25]}],"usage":{"total_tokens":2}}`)
	c, err := Decode(openai.FamilyEmbeddings, openai.FamilyEmbeddings, floats, "team-model", "base64")
	if err != nil {
		t.Fatal(err)
	}
	f, _ := object(c.Body)
	item, _ := object(arr(f["data"])[0])
	encoded, err := base64.StdEncoding.DecodeString(str(item["embedding"]))
	if err != nil || len(encoded) != 8 || math.Float32frombits(binary.LittleEndian.Uint32(encoded)) != 0.5 {
		t.Fatalf("vector %s %v", raw(item), err)
	}
	c, err = Decode(openai.FamilyEmbeddings, openai.FamilyEmbeddings, c.Body, "team-model", "float")
	if err != nil {
		t.Fatal(err)
	}
	f, _ = object(c.Body)
	item, _ = object(arr(f["data"])[0])
	var values []float64
	if json.Unmarshal(item["embedding"], &values) != nil || len(values) != 2 || values[1] != -1.25 {
		t.Fatalf("round trip %s", c.Body)
	}
}

func TestBedrockStreamTranslationAndAggregateToolBound(t *testing.T) {
	var encoded bytes.Buffer
	add := func(event string, value any) {
		t.Helper()
		h := eventstream.Headers{}
		h.Set(":message-type", eventstream.StringValue("event"))
		h.Set(":event-type", eventstream.StringValue(event))
		if err := eventstream.NewEncoder().Encode(&encoded, eventstream.Message{Headers: h, Payload: raw(value)}); err != nil {
			t.Fatal(err)
		}
	}
	add("messageStart", map[string]any{"role": "assistant"})
	add("contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]string{"text": "hello"}})
	add("contentBlockStop", map[string]int{"contentBlockIndex": 0})
	add("messageStop", map[string]string{"stopReason": "end_turn"})
	add("metadata", map[string]any{"usage": map[string]int{"inputTokens": 3, "outputTokens": 2, "totalTokens": 5}})
	for _, target := range []openai.Family{openai.FamilyChat, openai.FamilyResponses, openai.FamilyAnthropic, openai.FamilyGemini} {
		var out bytes.Buffer
		c, err := Stream("bedrock", target, bytes.NewReader(encoded.Bytes()), 8192, "team-model", true, func(b []byte) error { out.Write(b); return nil })
		if err != nil || c.Usage == nil || c.Usage.TotalTokens != 5 {
			t.Fatalf("%s: %#v %v", target, c, err)
		}
		if _, err = Stream(target, target, bytes.NewReader(out.Bytes()), 8192, "team-model", true, func([]byte) error { return nil }); err != nil {
			t.Fatalf("%s invalid stream %v: %s", target, err, out.String())
		}
	}
	var source strings.Builder
	frame := func(event string, v any) {
		source.WriteString("event: " + event + "\ndata: " + string(raw(v)) + "\n\n")
	}
	frame("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "a", "type": "message", "role": "assistant", "content": []any{}}})
	for i := 0; i < 2; i++ {
		frame("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": "call", "name": "tool", "input": map[string]any{}}})
		frame("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"x":"` + strings.Repeat("a", 550)}})
	}
	_, err := Stream(openai.FamilyAnthropic, openai.FamilyAnthropic, strings.NewReader(source.String()), 1024, "team-model", true, func([]byte) error { return nil })
	if !errors.Is(err, openai.ErrEventTooLarge) {
		t.Fatalf("aggregate retained state escaped its bound: %v", err)
	}
}

func TestCountingKeepsBedrockToolsAndInvalidGeneratedArgumentsRefuseTranslation(t *testing.T) {
	request, err := Parse(openai.FamilyInputTokens, []byte(`{"model":"team-model","input":"hello","tools":[{"type":"function","name":"weather","parameters":{"type":"object","properties":{}}}]}`), "team-model")
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := Encode(request, "bedrock", "amazon-bedrock", "wire-model", nil)
	if err != nil || !bytes.Contains(body, []byte("toolConfig")) || bytes.Contains(body, []byte("inferenceConfig")) {
		t.Fatalf("count input lost tool tokens: %s %v", body, err)
	}
	malformed := []byte(`{"id":"t","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call","type":"function","function":{"name":"weather","arguments":"not-json"}}]},"finish_reason":"tool_calls"}]}`)
	if _, err = Decode(openai.FamilyChat, openai.FamilyAnthropic, malformed, "team-model", ""); err == nil {
		t.Fatal("fabricated a native tool input from malformed generated arguments")
	}
}

func TestGeminiParallelResultsKeepDistinctCallRelationships(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"weather","args":{"city":"Paris"}}},{"functionCall":{"name":"weather","args":{"city":"London"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"weather","response":{"temperature":20}}},{"functionResponse":{"name":"weather","response":{"temperature":16}}}]}]}`)
	request, err := Parse(openai.FamilyGemini, input, "team-model")
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
		request, err = Parse(openai.FamilyGemini, bad, "team-model")
		if err != nil {
			continue
		}
		if _, _, err = Encode(request, "openai", "openai", "wire-model", nil); err == nil {
			t.Fatal("response schema was silently dropped")
		}
	}
}

func TestLaterChatCandidateWithInvalidToolArgumentsIsRefused(t *testing.T) {
	body := []byte(`{"id":"c","object":"chat.completion","model":"m","choices":[` +
		`{"index":0,"message":{"role":"assistant","content":"a"},"finish_reason":"stop"},` +
		`{"index":1,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"not-json"}}]},"finish_reason":"tool_calls"}]}`)
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("upstream candidate panicked translation: %v", p)
		}
	}()
	if _, err := Decode(openai.FamilyChat, openai.FamilyGemini, body, "route", ""); err == nil {
		t.Fatal("translated a later candidate with invalid tool arguments")
	}
}

// Anthropic streams a server tool's input with input_json_delta events, as in
// the documented web search stream.
func TestAnthropicServerToolInputStreamsNatively(t *testing.T) {
	var source strings.Builder
	for _, event := range []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":3,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"weather\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	} {
		f, _ := object([]byte(event))
		source.WriteString("event: " + str(f["type"]) + "\ndata: " + event + "\n\n")
	}
	var out bytes.Buffer
	c, err := Stream(openai.FamilyAnthropic, openai.FamilyAnthropic, strings.NewReader(source.String()), 1<<16, "route", true, func(frame []byte) error { out.Write(frame); return nil })
	if err != nil || c.Usage == nil || c.Usage.OutputTokens != 9 || !strings.Contains(out.String(), `"server_tool_use"`) {
		t.Fatalf("native server-tool stream: usage=%+v err=%v", c.Usage, err)
	}
	// A client function-call dialect has no server tool; translation still refuses.
	var translated bytes.Buffer
	if _, err = Stream(openai.FamilyAnthropic, openai.FamilyChat, strings.NewReader(source.String()), 1<<16, "route", true, func(frame []byte) error { translated.Write(frame); return nil }); err == nil || strings.Contains(translated.String(), "tool_calls") {
		t.Fatalf("server tool input became a client tool call: %v %s", err, translated.String())
	}
	incomplete := strings.Replace(source.String(), `{\"query\":\"weather\"}`, `{\"query\":`, 1)
	if _, err = Stream(openai.FamilyAnthropic, openai.FamilyAnthropic, strings.NewReader(incomplete), 1<<16, "route", true, func([]byte) error { return nil }); err == nil {
		t.Fatal("accepted incomplete server tool input")
	}
}
