package protocols

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

var generationFixtures = map[openai.Family]string{
	openai.FamilyChat:      `{"model":"team-model","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`,
	openai.FamilyResponses: `{"model":"team-model","input":"hello","max_output_tokens":32}`,
	openai.FamilyAnthropic: `{"model":"team-model","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`,
	openai.FamilyGemini:    `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":32}}`,
}

func TestMaintainedNonMediaRequestMatrix(t *testing.T) {
	for _, kind := range []string{"openai", "openai_compatible", "azure_openai", "anthropic", "gemini", "vertex_ai", "bedrock"} {
		for family, input := range generationFixtures {
			for _, mode := range []string{"unary", "streaming"} {
				t.Run(kind+"/"+string(family)+"/"+mode, func(t *testing.T) {
					if !connectors.Supports(kind, kind, "generation", family.Surface(), mode) {
						if kind != "openai_compatible" || family.Surface() == "openai" {
							t.Fatal("unexpected refusal")
						}
						return
					}
					fields, _ := object([]byte(input))
					source := family
					if mode == "streaming" {
						if family == openai.FamilyGemini {
							source = openai.FamilyGeminiStream
						} else {
							fields["stream"] = raw(true)
						}
					}
					r, e := Parse(source, raw(fields), "team-model")
					if e != nil {
						t.Fatal(e)
					}
					body, wire, e := Encode(r, kind, kind, "wire-model", nil)
					if e != nil {
						t.Fatal(e)
					}
					if !json.Valid(body) || wire == "" || bytes.Contains(body, []byte("team-model")) {
						t.Fatalf("invalid rewritten envelope: %s", body)
					}
				})
			}
		}
	}
	for _, kind := range []string{"openai", "openai_compatible", "azure_openai", "anthropic", "gemini", "vertex_ai", "bedrock"} {
		for family, input := range generationFixtures {
			count := map[openai.Family]openai.Family{openai.FamilyChat: openai.FamilyInputTokens, openai.FamilyResponses: openai.FamilyInputTokens, openai.FamilyAnthropic: openai.FamilyAnthropicCount, openai.FamilyGemini: openai.FamilyGeminiCount}[family]
			if family == openai.FamilyChat {
				input = `{"model":"team-model","input":"hello"}`
			}
			if !connectors.Supports(kind, kind, "token_count", count.Surface(), "unary") {
				continue
			}
			r, e := Parse(count, []byte(input), "team-model")
			if e != nil {
				t.Fatal(e)
			}
			_, wire, e := Encode(r, kind, kind, "wire-model", nil)
			if e != nil {
				t.Fatalf("%s count %s: %v", kind, family, e)
			}
			body := []byte(`{"input_tokens":13}`)
			if wire == openai.FamilyGeminiCount {
				body = []byte(`{"totalTokens":13}`)
			}
			if wire == "bedrock_count" {
				body = []byte(`{"inputTokens":13}`)
			}
			reply, e := Decode(wire, count, body, "team-model", "")
			if e != nil || reply.Usage == nil || reply.Usage.InputTokens != 13 {
				t.Fatalf("count %s/%s: %v", kind, family, e)
			}
		}
	}
	for _, kind := range []string{"openai", "openai_compatible", "azure_openai", "anthropic", "gemini", "vertex_ai", "bedrock"} {
		for _, operation := range []string{"embeddings", "moderation"} {
			want := kind == "openai" || kind == "openai_compatible" || kind == "azure_openai"
			if connectors.Supports(kind, kind, operation, "openai", "unary") != want {
				t.Fatal(kind, operation)
			}
			for _, surface := range []string{"anthropic", "gemini"} {
				if connectors.Supports(kind, kind, operation, surface, "unary") {
					t.Fatal("non-native operation broadened")
				}
			}
		}
		if connectors.Supports(kind, kind, "image_generation", "openai", "unary") {
			t.Fatal("media capability broadened")
		}
	}
}
func TestNativeExtensionsSurviveAndTranslationRefuses(t *testing.T) {
	for family, path := range map[openai.Family]string{openai.FamilyAnthropic: "anthropic-messages-request.json", openai.FamilyGemini: "gemini-generate-content-request.json"} {
		input, e := os.ReadFile("../../tests/fixtures/protocols/" + path)
		if e != nil {
			t.Fatal(e)
		}
		r, e := Parse(family, input, "team-model")
		if e != nil {
			t.Fatal(e)
		}
		kind := family.Surface()
		body, _, e := Encode(r, kind, kind, "wire-model", nil)
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Contains(body, []byte("vendor")) {
			t.Fatalf("lost native extension: %s", body)
		}
		if _, _, e = Encode(r, "openai", "openai", "wire-model", nil); e == nil {
			t.Fatal("silently dropped source extension")
		}
	}
}
func TestExplicitTranslationRefusals(t *testing.T) {
	for _, test := range []struct{ kind, field, value string }{{"anthropic", "seed", "7"}, {"anthropic", "response_format", `{"type":"json_object"}`}, {"gemini", "parallel_tool_calls", "true"}, {"bedrock", "seed", "7"}, {"bedrock", "n", "2"}, {"cohere", "parallel_tool_calls", "true"}} {
		fields, _ := object([]byte(generationFixtures[openai.FamilyChat]))
		fields[test.field] = json.RawMessage(test.value)
		r, e := Parse(openai.FamilyChat, raw(fields), "")
		if e != nil {
			t.Fatal(e)
		}
		kind := test.kind
		if kind == "cohere" {
			kind = "openai_compatible"
		}
		if _, _, e = Encode(r, kind, test.kind, "wire-model", nil); e == nil {
			t.Fatalf("accepted %s/%s", test.kind, test.field)
		}
	}
}
func TestResponseNativePreservationAndUsageCompleteness(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"wire-model","content":[{"type":"text","text":"hello","vendor_part":7}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2},"vendor_response":true}`)
	native, e := Decode(openai.FamilyAnthropic, openai.FamilyAnthropic, body, "team-model", "")
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(native.Body, []byte("vendor_part")) || !bytes.Contains(native.Body, []byte("vendor_response")) {
		t.Fatal("native fields lost")
	}
	translated, e := Decode(openai.FamilyAnthropic, openai.FamilyChat, body, "team-model", "")
	if e != nil || translated.OutputText != "hello" || translated.Usage.TotalTokens != 5 {
		t.Fatalf("translation: %+v %v", translated, e)
	}
	if bytes.Contains(translated.Body, []byte("vendor_")) {
		t.Fatal("unmodeled response extension escaped")
	}
	incomplete := bytes.Replace(body, []byte(`,"output_tokens":2`), nil, 1)
	c, e := Decode(openai.FamilyAnthropic, openai.FamilyChat, incomplete, "team-model", "")
	if e != nil || c.Usage != nil {
		t.Fatalf("invented complete usage: %+v %v", c, e)
	}
}
func TestNativeAndTranslatedStreams(t *testing.T) {
	sources := map[openai.Family]string{
		openai.FamilyChat:      "data: {\"id\":\"chat_1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"héllo 🌍\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n",
		openai.FamilyAnthropic: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"wire-model\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"héllo 🌍\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		openai.FamilyGemini:    "data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"héllo 🌍\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2,\"totalTokenCount\":5}}\n\n",
	}
	for wire, source := range sources {
		for _, target := range []openai.Family{openai.FamilyChat, openai.FamilyResponses, openai.FamilyAnthropic, openai.FamilyGemini} {
			t.Run(string(wire)+"/"+string(target), func(t *testing.T) {
				var result bytes.Buffer
				c, e := Stream(wire, target, strings.NewReader(source), 4096, "team-model", true, func(frame []byte) error { _, e := result.Write(frame); return e })
				if e != nil {
					t.Fatal(e)
				}
				if c.Usage == nil || c.Usage.TotalTokens != 5 || !strings.Contains(result.String(), "héllo 🌍") {
					t.Fatalf("bad stream: %+v %s", c, result.String())
				}
				if _, e = Stream(target, target, bytes.NewReader(result.Bytes()), 4096, "team-model", true, func([]byte) error { return nil }); e != nil {
					t.Fatalf("invalid translated event sequence: %v\n%s", e, result.String())
				}
			})
		}
		if _, e := Stream(wire, wire, strings.NewReader(source), 32, "team-model", true, func([]byte) error { return nil }); e == nil {
			t.Fatal("event bound not enforced")
		}
	}
	truncated := strings.Replace(sources[openai.FamilyAnthropic], "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "", 1)
	if _, e := Stream(openai.FamilyAnthropic, openai.FamilyChat, strings.NewReader(truncated), 4096, "team-model", true, func([]byte) error { return nil }); e == nil {
		t.Fatal("accepted truncated stream")
	}
	cancel := errors.New("downstream disconnected")
	if _, e := Stream(openai.FamilyGemini, openai.FamilyChat, strings.NewReader(sources[openai.FamilyGemini]), 4096, "team-model", true, func([]byte) error { return cancel }); !errors.Is(e, cancel) {
		t.Fatalf("lost cancellation: %v", e)
	}
}
func TestGeminiToolsNormalizeStopAndBedrockBoundPrecedesAllocation(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"weather","args":{"city":"Paris"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}`)
	c, e := Decode(openai.FamilyGemini, openai.FamilyChat, body, "team-model", "")
	if e != nil || c.FinishReason != "tool_calls" || len(c.ToolCalls) != 1 {
		t.Fatalf("tool completion: %+v %v", c, e)
	}
	prelude := make([]byte, 12)
	binary.BigEndian.PutUint32(prelude, 1<<30)
	if _, e = ReadBedrockEvent(bytes.NewReader(prelude), 4096); !errors.Is(e, openai.ErrEventTooLarge) {
		t.Fatalf("unbounded prelude: %v", e)
	}
	var encoded bytes.Buffer
	event := eventstream.Message{Payload: []byte(`{"role":"assistant"}`)}
	if e = eventstream.NewEncoder().Encode(&encoded, event); e != nil {
		t.Fatal(e)
	}
	encoded.Bytes()[encoded.Len()-1] ^= 1
	if _, e = ReadBedrockEvent(&encoded, 4096); e == nil {
		t.Fatal("accepted corrupt event CRC")
	}
}
func TestCallerDefaultsAndVendorProfiles(t *testing.T) {
	r, e := Parse(openai.FamilyResponses, []byte(`{"model":"team-model","input":"hello","max_output_tokens":17}`), "")
	if e != nil {
		t.Fatal(e)
	}
	body, wire, e := Encode(r, "openai_compatible", "deepseek", "wire-model", Object{"max_tokens": raw(90)})
	if e != nil || wire != openai.FamilyChat {
		t.Fatal(e)
	}
	f, _ := object(body)
	if string(f["max_tokens"]) != "17" || f["max_completion_tokens"] != nil {
		t.Fatalf("caller precedence: %s", body)
	}
	r, e = Parse(openai.FamilyEmbeddings, []byte(`{"model":"team-model","input":["a","b"],"dimensions":32,"encoding_format":"base64","truncation":false}`), "")
	if e != nil {
		t.Fatal(e)
	}
	body, _, e = Encode(r, "openai_compatible", "voyage", "voyage-model", nil)
	if e != nil || !bytes.Contains(body, []byte(`"output_dimension":32`)) || !bytes.Contains(body, []byte(`"truncation":false`)) {
		t.Fatalf("Voyage: %s %v", body, e)
	}
}
