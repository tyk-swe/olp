package protocols

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// The codecs are measured on what an agent sends and receives: a system prompt
// and a conversation that has run a tool, a tool catalogue, sampling controls,
// and an answer that states a tool call after some text.
const (
	benchChatRequest = `{"model":"team-chat","messages":[` +
		`{"role":"system","content":"You are a coding assistant. Answer briefly and use the tools to read files before you edit them."},` +
		`{"role":"user","content":"Why does the build fail on the config package?"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"config/config.go\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"package config\n\nfunc Load() (*Config, error) { return nil, nil }\n"},` +
		`{"role":"user","content":"Fix it and keep the change small."}],` +
		`"tools":[{"type":"function","function":{"name":"read_file","description":"Read a file from the workspace","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}},` +
		`{"type":"function","function":{"name":"write_file","description":"Replace the contents of a file","parameters":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}}}],` +
		`"tool_choice":"auto","temperature":0.2,"top_p":0.9,"max_tokens":1024}`

	benchChatResponse = `{"id":"chatcmpl-bench","object":"chat.completion","created":1800000000,"model":"gpt-bench","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"Load returns nil for both values, so I will return an empty configuration.",` +
		`"tool_calls":[{"id":"call_2","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"config/config.go\",\"content\":\"package config\\n\"}"}}]},"finish_reason":"tool_calls"}],` +
		`"usage":{"prompt_tokens":212,"completion_tokens":58,"total_tokens":270}}`

	benchAnthropicResponse = `{"id":"msg_bench","type":"message","role":"assistant","model":"claude-bench","content":[` +
		`{"type":"text","text":"Load returns nil for both values, so I will return an empty configuration."},` +
		`{"type":"tool_use","id":"toolu_2","name":"write_file","input":{"path":"config/config.go","content":"package config\n"}}],` +
		`"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":212,"output_tokens":58}}`

	benchGeminiResponse = `{"candidates":[{"index":0,"content":{"role":"model","parts":[` +
		`{"text":"Load returns nil for both values, so I will return an empty configuration."},` +
		`{"functionCall":{"name":"write_file","args":{"path":"config/config.go","content":"package config\n"}}}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":212,"candidatesTokenCount":58,"totalTokenCount":270},"modelVersion":"gemini-bench","responseId":"resp-bench"}`
)

// benchChunks is how many content frames the streams of the benchmarks carry:
// the answer of a model that writes a paragraph.
const benchChunks = 64

// benchStream is an answer of benchChunks frames in the dialect, which ends as the
// dialect ends a stream, with the usage it reports.
func benchStream(family openai.Family) []byte {
	var out bytes.Buffer
	switch family {
	case openai.FamilyChat:
		for i := range benchChunks {
			delta := `{"content":"word %d "}`
			if i == 0 {
				delta = `{"role":"assistant","content":"word %d "}`
			}
			fmt.Fprintf(&out, "data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"created\":1800000000,\"model\":\"gpt-bench\",\"choices\":[{\"index\":0,\"delta\":"+delta+",\"finish_reason\":null}]}\n\n", i)
		}
		out.WriteString("data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"created\":1800000000,\"model\":\"gpt-bench\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":24,\"completion_tokens\":64,\"total_tokens\":88}}\n\n")
		out.WriteString("data: [DONE]\n\n")
	case openai.FamilyAnthropic:
		out.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_bench\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-bench\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":24,\"output_tokens\":1}}}\n\n")
		out.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		for i := range benchChunks {
			fmt.Fprintf(&out, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"word %d \"}}\n\n", i)
		}
		out.WriteString("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		out.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":64}}\n\n")
		out.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	case openai.FamilyGemini:
		for i := range benchChunks {
			fmt.Fprintf(&out, "data: {\"responseId\":\"resp-bench\",\"modelVersion\":\"gemini-bench\",\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"word %d \"}]}}]}\n\n", i)
		}
		out.WriteString("data: {\"responseId\":\"resp-bench\",\"modelVersion\":\"gemini-bench\",\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"end\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":24,\"candidatesTokenCount\":64,\"totalTokenCount\":88}}\n\n")
	}
	return out.Bytes()
}

// BenchmarkTranslateRequest is the request path of a route whose target speaks
// another dialect than its caller: the caller's body parsed and validated, carried
// through the operation model, and encoded for the target. The native cases, which
// keep the caller's dialect, are what the translations are weighed against, and
// the path that most requests take: Claude Code speaks Anthropic to an Anthropic
// provider. The requests that come back are those the first cases produced, so
// that each direction is measured on the other's output.
//
//	go test ./internal/protocols -run '^$' -bench BenchmarkTranslateRequest -benchmem
func BenchmarkTranslateRequest(b *testing.B) {
	parsed, err := Parse(openai.FamilyChat, []byte(benchChatRequest), "")
	if err != nil {
		b.Fatal(err)
	}
	anthropic, _, err := Encode(parsed, "anthropic", "anthropic", "claude-bench", nil)
	if err != nil {
		b.Fatal(err)
	}
	gemini, _, err := Encode(parsed, "gemini", "gemini", "gemini-bench", nil)
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		family        openai.Family
		body          []byte
		kind, vendor  string
		model         string
		targetFamily  openai.Family
		targetContain string
	}{
		{"chat-to-chat", openai.FamilyChat, []byte(benchChatRequest), "openai", "openai", "gpt-bench", openai.FamilyChat, `"gpt-bench"`},
		{"chat-to-anthropic", openai.FamilyChat, []byte(benchChatRequest), "anthropic", "anthropic", "claude-bench", openai.FamilyAnthropic, `"tool_use"`},
		{"anthropic-to-chat", openai.FamilyAnthropic, anthropic, "openai", "openai", "gpt-bench", openai.FamilyChat, `"tool_calls"`},
		{"chat-to-gemini", openai.FamilyChat, []byte(benchChatRequest), "gemini", "gemini", "gemini-bench", openai.FamilyGemini, `"functionCall"`},
		{"gemini-to-chat", openai.FamilyGemini, gemini, "openai", "openai", "gpt-bench", openai.FamilyChat, `"tool_calls"`},
		// The target's model is not the one the body names, and a native Anthropic
		// request is given it, which is what tells the case from a body that was
		// passed along. A Gemini request names its model in the path, so it has none
		// to rewrite.
		{"anthropic-to-anthropic", openai.FamilyAnthropic, anthropic, "anthropic", "anthropic", "claude-routed", openai.FamilyAnthropic, `"model":"claude-routed"`},
		{"gemini-to-gemini", openai.FamilyGemini, gemini, "gemini", "gemini", "gemini-bench", openai.FamilyGemini, `"functionCall"`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			// The encoded body is checked once, outside the measurement, to be the
			// dialect and the conversation that the case names.
			request, err := Parse(tc.family, tc.body, "team-chat")
			if err != nil {
				b.Fatal(err)
			}
			encoded, wire, err := Encode(request, tc.kind, tc.vendor, tc.model, nil)
			if err != nil || wire != tc.targetFamily || !json.Valid(encoded) || !bytes.Contains(encoded, []byte(tc.targetContain)) {
				b.Fatalf("encoded %s as %s, %v: %.200s", tc.family, wire, err, encoded)
			}
			b.SetBytes(int64(len(tc.body)))
			b.ReportAllocs()
			for b.Loop() {
				request, err := Parse(tc.family, tc.body, "team-chat")
				if err != nil {
					b.Fatal(err)
				}
				if _, _, err := Encode(request, tc.kind, tc.vendor, tc.model, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkTranslateResponse is the unary answer of a target, validated in its own
// dialect and translated to the caller's, with the model rewritten to the route's.
// The native cases, which answer in the dialect they were given, check that
// rewrite, which is what tells them from a body that was passed along.
func BenchmarkTranslateResponse(b *testing.B) {
	for _, tc := range []struct {
		name         string
		wire, target openai.Family
		body         string
		contains     string
	}{
		{"chat-to-chat", openai.FamilyChat, openai.FamilyChat, benchChatResponse, `"team-chat"`},
		{"anthropic-to-chat", openai.FamilyAnthropic, openai.FamilyChat, benchAnthropicResponse, `"tool_calls"`},
		{"chat-to-anthropic", openai.FamilyChat, openai.FamilyAnthropic, benchChatResponse, `"tool_use"`},
		{"gemini-to-chat", openai.FamilyGemini, openai.FamilyChat, benchGeminiResponse, `"tool_calls"`},
		{"chat-to-gemini", openai.FamilyChat, openai.FamilyGemini, benchChatResponse, `"functionCall"`},
		{"anthropic-to-anthropic", openai.FamilyAnthropic, openai.FamilyAnthropic, benchAnthropicResponse, `"model":"team-chat"`},
		{"gemini-to-gemini", openai.FamilyGemini, openai.FamilyGemini, benchGeminiResponse, `"modelVersion":"team-chat"`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			completion, err := Decode(tc.wire, tc.target, []byte(tc.body), "team-chat", "")
			if err != nil || completion.Usage == nil || completion.Usage.TotalTokens != 270 || !bytes.Contains(completion.Body, []byte(tc.contains)) {
				b.Fatalf("decoded %s as %s, %v: %.200s", tc.wire, tc.target, err, completion.Body)
			}
			body := []byte(tc.body)
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Decode(tc.wire, tc.target, body, "team-chat", ""); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkTranslateStream is the relay of a stream of 64 content frames and its
// closing frames, each read from the target's event stream, validated, and written
// in the caller's dialect, as the gateway relays them. The native cases keep the
// dialect, and are what the translations are weighed against. The gateway hands
// the relay the family of the caller's request, which for a Gemini caller that
// streams is its own, so that is the target of the Gemini cases.
func BenchmarkTranslateStream(b *testing.B) {
	for _, tc := range []struct {
		name         string
		wire, target openai.Family
		contains     string
	}{
		{"chat-to-chat", openai.FamilyChat, openai.FamilyChat, "[DONE]"},
		{"anthropic-to-anthropic", openai.FamilyAnthropic, openai.FamilyAnthropic, "message_stop"},
		{"anthropic-to-chat", openai.FamilyAnthropic, openai.FamilyChat, "[DONE]"},
		{"chat-to-anthropic", openai.FamilyChat, openai.FamilyAnthropic, "message_stop"},
		{"gemini-to-chat", openai.FamilyGemini, openai.FamilyChat, "[DONE]"},
		{"chat-to-gemini", openai.FamilyChat, openai.FamilyGeminiStream, "finishReason"},
		{"gemini-to-gemini", openai.FamilyGemini, openai.FamilyGeminiStream, "finishReason"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			stream := benchStream(tc.wire)
			var frames int
			count := func([]byte) error { frames++; return nil }
			reader := bytes.NewReader(stream)
			relay := func(emit func([]byte) error) error {
				reader.Reset(stream)
				frames = 0
				_, err := Stream(tc.wire, tc.target, reader, 8192, "team-chat", true, emit)
				return err
			}
			// The relay is checked once, outside the measurement, to end as its
			// dialect ends a stream, with a frame for every chunk the target sent. A
			// frame is only good for the call that gets it, so the last is copied.
			var last []byte
			keep := func(frame []byte) error { frames++; last = append(last[:0], frame...); return nil }
			if err := relay(keep); err != nil || frames < benchChunks || !strings.Contains(string(last), tc.contains) {
				b.Fatalf("relayed %d frames ending %q, %v", frames, last, err)
			}
			b.SetBytes(int64(len(stream)))
			b.ReportAllocs()
			for b.Loop() {
				if err := relay(count); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(frames), "frames/op")
		})
	}
}
