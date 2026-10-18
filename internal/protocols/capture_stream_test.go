package protocols

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestCaptureCompleteNativeStreams(t *testing.T) {
	cases := []struct {
		name   string
		family wire.Format
		frames []string
		text   string
		tools  int
	}{
		{"chat", wire.Chat, []string{`{"choices":[{"index":0,"delta":{"content":"sec"}},{"index":1,"delta":{"content":"other"}}]}`, `{"choices":[{"index":1,"delta":{},"finish_reason":"stop"},{"index":0,"delta":{"content":"ret"},"finish_reason":"stop"}]}`}, "secret\nother", 0},
		{"mistral", wire.MistralFIM, []string{`{"choices":[{"index":0,"delta":{"content":"sec"}}]}`, `{"choices":[{"index":0,"delta":{"content":"ret"},"finish_reason":"stop"}]}`}, "secret", 0},
		{"responses", wire.Responses, []string{
			`{"type":"response.output_item.added","item":{"id":"fc","type":"function_call","name":"f","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc","delta":"{\"x\":\"sec"}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc","delta":"ret\"}"}`,
			`{"type":"response.function_call_arguments.done","item_id":"fc","arguments":"{\"x\":\"secret\"}"}`,
			`{"type":"response.output_item.done","item":{"id":"fc","type":"function_call","name":"f","arguments":"{\"x\":\"secret\"}"}}`,
			`{"type":"response.output_text.delta","item_id":"m","content_index":0,"delta":"sec"}`,
			`{"type":"response.output_text.delta","item_id":"m","content_index":0,"delta":"ret"}`,
			`{"type":"response.output_text.done","item_id":"m","content_index":0,"text":"secret"}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"id":"fc","type":"function_call","name":"f","arguments":"{\"x\":\"secret\"}"},{"id":"m","type":"message","content":[{"type":"output_text","text":"secret"}]}]}}`,
		}, "secret", 1},
		{"anthropic", wire.Anthropic, []string{
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"secret"}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"a","name":"f","input":{ }}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\":\"sec"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"ret\"}"}}`,
			`{"type":"content_block_stop","index":1}`, `{"type":"content_block_stop","index":0}`, `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`, `{"type":"message_stop"}`,
		}, "secret", 1},
		{"gemini", wire.GeminiStream, []string{
			`{"candidates":[{"index":2,"content":{"parts":[{"text":"sec"}]}}]}`,
			`{"candidates":[{"index":2,"content":{"parts":[{"text":"ret"},{"functionCall":{"id":"c","name":"f","args":{"x":"secret"}}}]},"finishReason":"STOP"}]}`,
		}, "secret", 1},
		{"cohere", wire.CohereChat, []string{
			`{"type":"message-start"}`, `{"type":"content-start","index":0}`,
			`{"type":"content-delta","index":0,"delta":{"message":{"content":{"text":"secret"}}}}`,
			`{"type":"tool-call-start","index":0,"delta":{"message":{"tool_calls":{"function":{"name":"f","arguments":"{\"x\":\"sec"}}}}}`,
			`{"type":"tool-call-start","index":1,"delta":{"message":{"tool_calls":{"function":{"name":"g","arguments":"{\"x\":\"other"}}}}}`,
			`{"type":"tool-call-delta","index":1,"delta":{"message":{"tool_calls":{"function":{"arguments":"\"}"}}}}}`,
			`{"type":"tool-call-delta","index":0,"delta":{"message":{"tool_calls":{"function":{"arguments":"ret\"}"}}}}}`,
			`{"type":"tool-call-end","index":0}`, `{"type":"tool-call-end","index":1}`, `{"type":"content-end","index":0}`, `{"type":"message-end","delta":{"finish_reason":"TOOL_CALL"}}`,
		}, "secret", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := NewCaptureStream(tc.family, 65536, true, true)
			for _, f := range tc.frames {
				frame := []byte("data: " + f + "\n\n")
				original := string(frame)
				if err := stream.Push(frame); err != nil {
					t.Fatalf("frame %s: %v", f, err)
				}
				if string(frame) != original {
					t.Fatal("inference frame changed")
				}
			}
			if tc.family == wire.Chat || tc.family == wire.MistralFIM {
				if err := stream.Push([]byte("data: [DONE]\n\n")); err != nil {
					t.Fatal(err)
				}
			}
			text, calls, err := stream.Finish()
			if err != nil || text != tc.text || len(calls) != tc.tools {
				t.Fatalf("finish text=%q calls=%d err=%v", text, len(calls), err)
			}
			for _, raw := range calls {
				if !json.Valid(raw) {
					t.Fatal("invalid completed tool JSON")
				}
			}
		})
	}
}
func TestCaptureBedrockCompleteStream(t *testing.T) {
	stream := NewCaptureStream(wire.Bedrock, 65536, true, true)
	for _, event := range [][2]string{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"secret"}}`},
		{"contentBlockStart", `{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"c","name":"f"}}}`},
		{"contentBlockDelta", `{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"x\":\"sec"}}}`},
		{"contentBlockDelta", `{"contentBlockIndex":1,"delta":{"toolUse":{"input":"ret\"}"}}}`},
		{"contentBlockStop", `{"contentBlockIndex":0}`}, {"contentBlockStop", `{"contentBlockIndex":1}`},
		{"messageStop", `{"stopReason":"tool_use"}`},
	} {
		headers := eventstream.Headers{}
		headers.Set(":message-type", eventstream.StringValue("event"))
		headers.Set(":event-type", eventstream.StringValue(event[0]))
		var frame bytes.Buffer
		if err := eventstream.NewEncoder().Encode(&frame, eventstream.Message{Headers: headers, Payload: []byte(event[1])}); err != nil {
			t.Fatal(err)
		}
		if err := stream.Push(frame.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	text, calls, err := stream.Finish()
	if err != nil || text != "secret" || len(calls) != 1 || !strings.Contains(string(calls[0]), "secret") {
		t.Fatalf("bedrock text=%q calls=%s err=%v", text, calls, err)
	}
}

func TestCaptureFragmentedChatToolNames(t *testing.T) {
	for _, family := range []wire.Format{wire.Chat, wire.MistralFIM} {
		for _, field := range []string{"tool_calls", "function_call"} {
			t.Run(string(family)+"/"+field, func(t *testing.T) {
				s := NewCaptureStream(family, 65536, false, true)
				for i, name := range []string{"wea", "ther"} {
					function := map[string]any{"name": name}
					choice := map[string]any{"index": 0}
					if i == 1 {
						function["arguments"] = `{}`
						choice["finish_reason"] = field
					}
					var call any = function
					if field == "tool_calls" {
						call = []any{map[string]any{"index": 0, "function": function}}
					}
					choice["delta"] = map[string]any{field: call}
					raw, _ := json.Marshal(map[string]any{"choices": []any{choice}})
					if err := s.Push(append(append([]byte("data: "), raw...), '\n', '\n')); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.Push([]byte("data: [DONE]\n\n")); err != nil {
					t.Fatal(err)
				}
				_, calls, err := s.Finish()
				if err != nil || len(calls) != 1 || !strings.Contains(string(calls[0]), `"name":"weather"`) {
					t.Fatalf("fragmented name: %s %v", calls, err)
				}
			})
		}
	}
}

func TestCaptureGeminiOptionalIdentities(t *testing.T) {
	for _, same := range []bool{false, true} {
		s := NewCaptureStream(wire.GeminiStream, 65536, true, true)
		for i := range 3 {
			argument := i
			if same {
				argument = 0
			}
			call := map[string]any{"name": "f", "args": map[string]any{"n": argument}}
			if i == 2 {
				// An explicit ID cannot collide with an anonymous call's identity.
				call["id"] = "anonymous/0"
			}
			candidate := map[string]any{"content": map[string]any{"parts": []any{map[string]any{"functionCall": call}}}}
			if i == 2 {
				candidate["finishReason"] = "STOP"
			}
			raw, _ := json.Marshal(map[string]any{"candidates": []any{candidate}})
			if err := s.Push(append(append([]byte("data: "), raw...), '\n', '\n')); err != nil {
				t.Fatal(err)
			}
		}
		_, calls, err := s.Finish()
		if err != nil || len(calls) != 3 {
			t.Fatalf("same=%t: calls=%s err=%v", same, calls, err)
		}
		seen := map[string]bool{}
		for _, raw := range calls {
			var call struct{ ID string }
			if err := json.Unmarshal(raw, &call); err != nil || seen[call.ID] {
				t.Fatalf("duplicate or malformed call: %s", raw)
			}
			seen[call.ID] = true
		}
	}
	for _, index := range []string{`null`, `"0"`, `-1`, `0.5`} {
		s := NewCaptureStream(wire.GeminiStream, 65536, true, false)
		_ = s.Push([]byte(`data: {"candidates":[{"index":` + index + `,"content":{"parts":[{"text":"bad"}]},"finishReason":"STOP"}]}` + "\n\n"))
		if _, _, err := s.Finish(); err == nil {
			t.Fatalf("accepted malformed explicit index %s", index)
		}
	}
}

func TestCaptureGeminiUnselectedOpaqueOutput(t *testing.T) {
	for _, output := range []bool{false, true} {
		s := NewCaptureStream(wire.GeminiStream, 65536, output, true)
		_ = s.Push([]byte("data: " + `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"opaque-sentinel"}},{"executableCode":{"language":"PYTHON","code":"opaque-sentinel"}},{"functionCall":{"name":"f","args":{"value":"secret"}}}]},"finishReason":"STOP"}]}` + "\n\n"))
		text, calls, err := s.Finish()
		if output {
			if err == nil {
				t.Fatal("selected opaque output accepted")
			}
		} else if err != nil || text != "" || len(calls) != 1 || strings.Contains(string(calls[0]), "opaque-sentinel") {
			t.Fatalf("unselected output affected tools: %q %s %v", text, calls, err)
		}
	}
}

func TestCaptureSupportedLifecycleAndFeedback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family wire.Format
		frames []string
	}{
		{"responses-queued", wire.Responses, []string{
			`{"type":"response.queued","response":{"status":"queued"}}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"id":"m","type":"message","content":[{"type":"output_text","text":"secret"}]}]}}`,
		}},
		{"responses-omitted-status", wire.Responses, []string{
			`{"type":"response.output_text.delta","item_id":"m","content_index":0,"delta":"secret"}`,
			`{"type":"response.output_text.annotation.added","item_id":"m","content_index":0,"annotation_index":0,"annotation":{"type":"url_citation","url":"https://example.com","title":"excluded citation"}}`,
			`{"type":"response.completed","response":{"output":[{"id":"m","type":"message","content":[{"type":"output_text","text":"secret"}]}]}}`,
		}},
		{"cohere-debug", wire.CohereChat, []string{
			`{"type":"content-start","index":0}`,
			`{"type":"content-delta","index":0,"delta":{"message":{"content":{"text":"secret"}}}}`,
			`{"type":"debug","message":"diagnostic data must stay excluded"}`,
			`{"type":"content-end","index":0}`,
			`{"type":"message-end","delta":{"finish_reason":"COMPLETE"}}`,
		}},
		{"gemini-safety-ratings", wire.GeminiStream, []string{
			`{"promptFeedback":{"safetyRatings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","probability":"NEGLIGIBLE"}]},"candidates":[{"content":{"parts":[{"text":"secret"}]},"finishReason":"STOP"}]}`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewCaptureStream(tc.family, 65536, true, true)
			for _, frame := range tc.frames {
				if err := s.Push([]byte("data: " + frame + "\n\n")); err != nil {
					t.Fatal(err)
				}
			}
			text, calls, err := s.Finish()
			if err != nil || text != "secret" || len(calls) != 0 {
				t.Fatalf("finish: %q %s %v", text, calls, err)
			}
		})
	}
	for _, status := range []string{`null`, `123`, `"in_progress"`, `"failed"`} {
		s := NewCaptureStream(wire.Responses, 65536, true, true)
		_ = s.Push([]byte(`data: {"type":"response.completed","response":{"status":` + status + `,"output":[]}}` + "\n\n"))
		if _, _, err := s.Finish(); err == nil {
			t.Fatalf("accepted invalid terminal status %s", status)
		}
	}
	for _, frame := range []string{
		`{"promptFeedback":{"blockReason":"SAFETY"}}`,
		`{"promptFeedback":{"blockReason":"SAFETY"},"candidates":[{"content":{"parts":[{"text":"secret"}]},"finishReason":"STOP"}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"secret"}]},"finishReason":"SAFETY"}]}`,
	} {
		s := NewCaptureStream(wire.GeminiStream, 65536, true, true)
		_ = s.Push([]byte("data: " + frame + "\n\n"))
		if _, _, err := s.Finish(); err == nil {
			t.Fatalf("accepted blocked Gemini response %s", frame)
		}
	}
}
func TestCaptureStreamDropsIncompleteMalformedAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family wire.Format
		frames []string
		limit  int
	}{
		{"no-finish", wire.Chat, []string{`{"choices":[{"index":0,"delta":{"content":"sec"}}]}`}, 65536},
		{"no-terminal", wire.Chat, []string{`{"choices":[{"index":0,"delta":{"content":"secret"},"finish_reason":"stop"}]}`}, 65536},
		{"mistral-no-terminal", wire.MistralFIM, []string{`{"choices":[{"index":0,"delta":{"content":"secret"},"finish_reason":"stop"}]}`}, 65536},
		{"early-terminal", wire.Chat, []string{`[DONE]`, `{"choices":[{"index":0,"delta":{"content":"secret"},"finish_reason":"stop"}]}`}, 65536},
		{"done-without-choice-finish", wire.Chat, []string{`{"choices":[{"index":0,"delta":{"content":"sec"}}]}`, `[DONE]`}, 65536},
		{"malformed", wire.Chat, []string{`{broken`}, 65536},
		{"truncated", wire.Chat, []string{`{"choices":[{"index":0,"delta":{"content":"sec"},"finish_reason":"length"}]}`}, 65536},
		{"identity", wire.Chat, []string{`{"choices":[{"delta":{"content":"secret"},"finish_reason":"stop"}]}`}, 65536},
		{"tool-json", wire.Chat, []string{`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"{broken"}}]},"finish_reason":"tool_calls"}]}`}, 65536},
		{"bound", wire.Chat, []string{`{"choices":[{"index":0,"delta":{"content":"secret"},"finish_reason":"stop"}]}`}, 4},
		{"unsupported", wire.GeminiInteractions, []string{`{}`}, 65536},
		{"unfinished-block", wire.Anthropic, []string{`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"sec"}}`, `{"type":"message_stop"}`}, 65536},
		{"conflicting-snapshot", wire.Responses, []string{`{"type":"response.output_text.delta","item_id":"m","content_index":0,"delta":"secret"}`, `{"type":"response.output_text.done","item_id":"m","content_index":0,"text":"different"}`}, 65536},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewCaptureStream(tc.family, tc.limit, true, true)
			for _, f := range tc.frames {
				_ = s.Push([]byte("data: " + f + "\n\n"))
			}
			if _, _, err := s.Finish(); err == nil {
				t.Fatal("unsafe capture accepted")
			}
		})
	}
}

func BenchmarkCaptureStreamSmallFragments(b *testing.B) {
	const outputSize = 60 << 10
	frame := []byte("data: " + `{"choices":[{"index":0,"delta":{"content":"text"}}]}` + "\n\n")
	end := []byte("data: " + `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n")
	b.ReportAllocs()
	b.SetBytes(outputSize)
	for b.Loop() {
		s := NewCaptureStream(wire.Chat, 65536, true, false)
		for range outputSize / 4 {
			if err := s.Push(frame); err != nil {
				b.Fatal(err)
			}
		}
		if err := s.Push(end); err != nil {
			b.Fatal(err)
		}
		text, _, err := s.Finish()
		if err != nil || len(text) != outputSize {
			b.Fatalf("incomplete capture: %d %v", len(text), err)
		}
	}
}
func TestCaptureUnaryNativeText(t *testing.T) {
	for _, tc := range []struct {
		family wire.Format
		body   string
	}{
		{wire.Bedrock, `{"stopReason":"end_turn","output":{"message":{"content":[{"text":"secret"}]}}}`},
		{wire.CohereChat, `{"finish_reason":"COMPLETE","message":{"content":[{"type":"text","text":"secret"}]}}`},
		{wire.MistralFIM, `{"choices":[{"finish_reason":"stop","message":{"content":"secret"}}]}`},
	} {
		text, _, err := CaptureResponse(tc.family, []byte(tc.body), true, true)
		if err != nil || text != "secret" {
			t.Fatalf("%s: %q %v", tc.family, text, err)
		}
	}
}

func TestCaptureMalformedNativeFieldTypes(t *testing.T) {
	for _, raw := range []string{
		`{"choices":[{"index":0,"delta":{"content":123},"finish_reason":"stop"}]}`,
		`{"choices":[{"index":0,"delta":{"content":"\ud800"},"finish_reason":"stop"}]}`,
		`{"choices":[{"index":0,"delta":{"content":"secret","content":"safe"},"finish_reason":"stop"}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":{}},"finish_reason":"stop"}]}`,
	} {
		s := NewCaptureStream(wire.Chat, 65536, true, true)
		_ = s.Push([]byte("data: " + raw + "\n\n"))
		if _, _, err := s.Finish(); err == nil {
			t.Fatal("malformed field accepted")
		}
	}
	s := NewCaptureStream(wire.Responses, 65536, true, true)
	_ = s.Push([]byte("data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"content_index\":0,\"delta\":\"partial\"}\n\n"))
	_ = s.Push([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"))
	if _, _, err := s.Finish(); err == nil {
		t.Fatal("terminal event completed an unfinished text item")
	}
	for _, raw := range []string{`{"choices":{}}`, `{"choices":[{"message":{"content":5}}]}`, `{"choices":[{"message":{"tool_calls":[{"function":{"name":"f","arguments":"{bad"}}]}}]}`} {
		if _, _, err := CaptureResponse(wire.Chat, []byte(raw), true, true); err == nil {
			t.Fatal("malformed unary content accepted")
		}
	}
}
