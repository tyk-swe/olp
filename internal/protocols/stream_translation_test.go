package protocols

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/sse"
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
			deltas := func() string {
				if fragmented {
					return "data: " + `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call-read","name":"read_file","arguments":""}}` + "\n\ndata: " + `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":"}` + "\n\ndata: " + `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"main.go\"}"}` + "\n\ndata: " + `{"type":"response.output_item.done","output_index":0,"item":` + item + `}` + "\n\n"
				}
				return ""
			}()
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
	for _, target := range []wire.Format{wire.Anthropic, wire.Chat} {
		body := `{"id":"opaque","status":"completed","output":[{"type":"reasoning","encrypted_content":"opaque-native-state"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
		if _, err := Decode(wire.Responses, target, []byte(body), "route", ""); err == nil {
			t.Fatal("opaque reasoning silently disappeared", target)
		}
		stream := "data: " + `{"type":"response.completed","response":` + body + `}` + "\n\n"
		if _, err := AggregateTranslatedResponses(target, strings.NewReader(stream), 8192, "route", nil); err == nil {
			t.Fatal("aggregated opaque reasoning silently disappeared", target)
		}
		var frames bytes.Buffer
		if _, err := Stream(wire.Responses, target, strings.NewReader(stream), 8192, "route", true, func(frame []byte) error { _, err := frames.Write(frame); return err }); err == nil || bytes.Contains(frames.Bytes(), []byte("[DONE]")) {
			t.Fatal("streamed opaque reasoning silently disappeared", target)
		}
	}
}

func TestResponsesCompletedMessagesSurviveStreamingAndAggregation(t *testing.T) {
	source := "data: " + `{"type":"response.created","response":{"id":"completed-messages","status":"in_progress","output":[]}}` + "\n\ndata: " + `{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hello"}` + "\n\ndata: " + `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello "}]}}` + "\n\ndata: " + `{"type":"response.output_item.done","output_index":1,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"back"}]}}` + "\n\ndata: " + `{"type":"response.completed","response":{"id":"completed-messages","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}` + "\n\n"
	unary, err := AggregateTranslatedResponses(wire.Anthropic, strings.NewReader(source), 8192, "route", nil)
	if err != nil {
		t.Fatal(err)
	}
	if unary.OutputText != "Hello back" || unary.FinishReason != "stop" || unary.Usage == nil || unary.Usage.TotalTokens != 9 {
		t.Fatalf("completed message %+v", unary)
	}
	var frames bytes.Buffer
	if _, err := Stream(wire.Responses, wire.Anthropic, strings.NewReader(source), 8192, "route", true, func(frame []byte) error { frames.Write(frame); return nil }); err != nil {
		t.Fatal(err)
	}
	capture := NewCaptureStream(wire.Anthropic, 8192, true, false)
	if err := capture.Push(frames.Bytes()); err != nil {
		t.Fatal(err)
	}
	text, _, err := capture.Finish()
	if err != nil || text != unary.OutputText {
		t.Fatalf("streamed text %q error %v frames %s", text, err, frames.String())
	}
}

func TestResponsesRefusalSnapshotsKeepClassification(t *testing.T) {
	const response = `{"id":"refusal","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"Cannot help"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
	for _, target := range []wire.Format{wire.Chat, wire.Anthropic} {
		for _, fragmented := range []bool{false, true} {
			for _, completedItem := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/fragments-%t/item-%t", target, fragmented, completedItem), func(t *testing.T) {
					delta := func() string {
						if fragmented {
							return `data: {"type":"response.refusal.delta","output_index":0,"delta":"Cannot "}` + "\n\n"
						}
						return ""
					}()
					item := func() string {
						if completedItem {
							return `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"Cannot help"}]}}` + "\n\n"
						}
						return ""
					}()
					events := `data: {"type":"response.created","response":{"id":"refusal","status":"in_progress","output":[]}}` + "\n\n" + delta + item + `data: {"type":"response.completed","response":` + response + `}` + "\n\n"
					assertBody := func(body []byte) {
						t.Helper()
						if target == wire.Chat {
							var got struct {
								Choices []struct {
									Message struct{ Content, Refusal string }
								}
							}
							if err := json.Unmarshal(body, &got); err != nil || len(got.Choices) != 1 || got.Choices[0].Message.Refusal != "Cannot help" || got.Choices[0].Message.Content != "" {
								t.Fatalf("refusal changed %s %v", body, err)
							}
							return
						}
						var got struct {
							Content    []struct{ Text string }
							StopReason string `json:"stop_reason"`
						}
						if err := json.Unmarshal(body, &got); err != nil || len(got.Content) != 1 || got.Content[0].Text != "Cannot help" || got.StopReason != "refusal" {
							t.Fatalf("refusal changed %s %v", body, err)
						}
					}
					unary, err := Decode(wire.Responses, target, []byte(response), "route", "")
					if err != nil {
						t.Fatal(err)
					}
					assertBody(unary.Body)
					aggregated, err := AggregateTranslatedResponses(target, strings.NewReader(events), 8192, "route", nil)
					if err != nil {
						t.Fatal(err)
					}
					assertBody(aggregated.Body)
					var emitted bytes.Buffer
					if _, err := Stream(wire.Responses, target, strings.NewReader(events), 8192, "route", true, func(frame []byte) error { _, err := emitted.Write(frame); return err }); err != nil {
						t.Fatal(err)
					}
					var text strings.Builder
					streamErr := sse.Decode(strings.NewReader(emitted.String()), 8192, func(frame sse.Frame) error {
						if frame.Data == "[DONE]" {
							return nil
						}
						var event struct {
							Delta   struct{ Text string }
							Choices []struct{ Delta struct{ Refusal string } }
						}
						if err := json.Unmarshal([]byte(frame.Data), &event); err != nil {
							return err
						}
						if target == wire.Anthropic {
							text.WriteString(event.Delta.Text)
							return nil
						}
						for _, choice := range event.Choices {
							text.WriteString(choice.Delta.Refusal)
						}
						return nil
					})
					if streamErr != nil || text.String() != "Cannot help" {
						t.Fatalf("refusal stream changed %s %v", emitted.Bytes(), streamErr)
					}
					if target == wire.Chat && (bytes.Contains(emitted.Bytes(), []byte(`"content":`)) || !bytes.Contains(emitted.Bytes(), []byte(`"refusal":`))) {
						t.Fatalf("refusal became content %s", emitted.Bytes())
					}
					if target == wire.Anthropic && !bytes.Contains(emitted.Bytes(), []byte(`"stop_reason":"refusal"`)) {
						t.Fatalf("refusal became successful text %s", emitted.Bytes())
					}
				})
			}
		}
	}
}
