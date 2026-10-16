package protocols

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/connectors"
)

var generationFixtures = map[wire.Format]string{
	wire.Chat:      `{"model":"team-model","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`,
	wire.Responses: `{"model":"team-model","input":"hello","max_output_tokens":32}`,
	wire.Anthropic: `{"model":"team-model","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`,
	wire.Gemini:    `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":32}}`,
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
						if family == wire.Gemini {
							source = wire.GeminiStream
						} else {
							fields["stream"] = raw(true)
						}
					}
					r, e := Parse(source, raw(fields), "team-model")
					if e != nil {
						t.Fatal(e)
					}
					body, encodedWire, e := Encode(r, kind, kind, "wire-model", nil)
					if e != nil {
						t.Fatal(e)
					}
					if !json.Valid(body) || encodedWire == "" || bytes.Contains(body, []byte("team-model")) {
						t.Fatalf("invalid rewritten envelope: %s", body)
					}
				})
			}
		}
	}
	for _, kind := range []string{"openai", "openai_compatible", "azure_openai", "anthropic", "gemini", "vertex_ai", "bedrock"} {
		for family, input := range generationFixtures {
			count := map[wire.Format]wire.Format{wire.Chat: wire.InputTokens, wire.Responses: wire.InputTokens, wire.Anthropic: wire.AnthropicCount, wire.Gemini: wire.GeminiCount}[family]
			if family == wire.Chat {
				input = `{"model":"team-model","input":"hello"}`
			}
			if !connectors.Supports(kind, kind, "token_count", count.Surface(), "unary") {
				continue
			}
			r, e := Parse(count, []byte(input), "team-model")
			if e != nil {
				t.Fatal(e)
			}
			_, encodedWire, e := Encode(r, kind, kind, "wire-model", nil)
			if e != nil {
				t.Fatalf("%s count %s: %v", kind, family, e)
			}
			body := []byte(`{"input_tokens":13}`)
			if encodedWire == wire.GeminiCount {
				body = []byte(`{"totalTokens":13}`)
			}
			if encodedWire == "bedrock_count" {
				body = []byte(`{"inputTokens":13}`)
			}
			reply, e := Decode(encodedWire, count, body, "team-model", "")
			if e != nil || reply.Usage == nil || reply.Usage.InputTokens != 13 {
				t.Fatalf("count %s/%s: %v", kind, family, e)
			}
		}
	}
	for _, kind := range []string{"openai", "openai_compatible", "azure_openai", "anthropic", "gemini", "vertex_ai", "bedrock"} {
		for _, operation := range []string{"embeddings", "moderation"} {
			want := kind == "openai" || kind == "openai_compatible" || kind == "azure_openai"
			if operation == "embeddings" {
				want = want || kind == "gemini" || kind == "vertex_ai" || kind == "bedrock"
			}
			if connectors.Supports(kind, kind, operation, "openai", "unary") != want {
				t.Fatal(kind, operation)
			}
			for _, surface := range []string{"anthropic", "gemini"} {
				if connectors.Supports(kind, kind, operation, surface, "unary") {
					t.Fatal("non-native operation broadened")
				}
			}
		}
		for _, operation := range []string{"image_generation", "image_edit", "speech", "transcription"} {
			want := kind == "openai" || kind == "openai_compatible" || kind == "azure_openai"
			for _, mode := range []string{"unary", "streaming"} {
				modeWant := want
				if operation == "image_generation" && mode == "unary" {
					modeWant = modeWant || kind == "vertex_ai" || kind == "bedrock"
				}
				if connectors.Supports(kind, kind, operation, "openai", mode) != modeWant {
					t.Fatal(kind, operation, mode)
				}
			}
			for _, surface := range []string{"anthropic", "gemini"} {
				if connectors.Supports(kind, kind, operation, surface, "unary") {
					t.Fatal("non-native media operation broadened")
				}
			}
		}
		if connectors.Supports(kind, kind, "image_generation", "openai", "async") {
			t.Fatal("image generation admitted async mode")
		}
		want := kind == "openai" || kind == "openai_compatible" || kind == "azure_openai"
		if connectors.Supports(kind, kind, "image_variation", "openai", "unary") != want ||
			connectors.Supports(kind, kind, "image_variation", "openai", "streaming") {
			t.Fatal(kind, "image_variation")
		}
		if connectors.Supports(kind, kind, "video_create", "openai", "async") != want ||
			connectors.Supports(kind, kind, "video_create", "openai", "unary") {
			t.Fatal(kind, "video_create")
		}
		for _, operation := range []string{"video_list", "video_get", "video_content", "video_delete"} {
			if connectors.Supports(kind, kind, operation, "openai", "unary") != want {
				t.Fatal(kind, operation)
			}
		}
	}
}

func TestNativeAndTranslatedStreams(t *testing.T) {
	sources := map[wire.Format]string{
		wire.Chat:      "data: {\"id\":\"chat_1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"héllo 🌍\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n",
		wire.Anthropic: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"wire-model\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"héllo 🌍\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		wire.Gemini:    "data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"héllo 🌍\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2,\"totalTokenCount\":5}}\n\n",
	}
	for encodedWire, source := range sources {
		for _, target := range []wire.Format{wire.Chat, wire.Responses, wire.Anthropic, wire.Gemini} {
			t.Run(string(encodedWire)+"/"+string(target), func(t *testing.T) {
				var result bytes.Buffer
				c, e := Stream(encodedWire, target, strings.NewReader(source), 4096, "team-model", true, func(frame []byte) error { _, e := result.Write(frame); return e })
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
		if _, e := Stream(encodedWire, encodedWire, strings.NewReader(source), 32, "team-model", true, func([]byte) error { return nil }); e == nil {
			t.Fatal("event bound not enforced")
		}
	}
	truncated := strings.Replace(sources[wire.Anthropic], "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "", 1)
	if _, e := Stream(wire.Anthropic, wire.Chat, strings.NewReader(truncated), 4096, "team-model", true, func([]byte) error { return nil }); e == nil {
		t.Fatal("accepted truncated stream")
	}
	cancel := errors.New("downstream disconnected")
	if _, e := Stream(wire.Gemini, wire.Chat, strings.NewReader(sources[wire.Gemini]), 4096, "team-model", true, func([]byte) error { return cancel }); !errors.Is(e, cancel) {
		t.Fatalf("lost cancellation: %v", e)
	}
}
