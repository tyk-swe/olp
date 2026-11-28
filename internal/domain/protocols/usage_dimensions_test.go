package protocols

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

func TestGeminiUsageModalityAndGrounding(t *testing.T) {
	body := `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP",
			"groundingMetadata":{"webSearchQueries":["a","b"]}},
			{"index":1,"content":{"parts":[{"text":"ho"}]},"finishReason":"STOP",
			"groundingMetadata":{"webSearchQueries":["c"]}}],
		"usageMetadata":{"promptTokenCount":40,"candidatesTokenCount":10,"totalTokenCount":50,
			"promptTokensDetails":[{"modality":"TEXT","tokenCount":30},{"modality":"AUDIO","tokenCount":7},{"modality":"IMAGE","tokenCount":2},{"modality":"VIDEO","tokenCount":1}],
			"cacheTokensDetails":[{"modality":"AUDIO","tokenCount":3},{"modality":"VIDEO","tokenCount":2}],
			"candidatesTokensDetails":[{"modality":"AUDIO","tokenCount":4},{"modality":"IMAGE","tokenCount":1}]}}`
	c, err := decodeGemini([]byte(body), "gemini", false)
	if err != nil {
		t.Fatal(err)
	}
	u := c.Usage
	checks := []struct {
		got  *int64
		want int64
	}{
		{u.AudioInputTokens, 7}, {u.ImageInputTokens, 3},
		{u.CachedAudioInputTokens, 3}, {u.CachedImageInputTokens, 2},
		{u.AudioOutputTokens, 4}, {u.ImageOutputTokens, 1},
	}
	for _, check := range checks {
		if check.got == nil || *check.got != check.want {
			t.Fatalf("usage %+v", u)
		}
	}
	want := map[string]int64{"google_search_query": 3, "google_search_prompt": 1}
	if !reflect.DeepEqual(u.ToolCalls, want) {
		t.Fatalf("tool calls %v, want %v", u.ToolCalls, want)
	}
	overflow := `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":40,"candidatesTokenCount":10,"totalTokenCount":50,
			"promptTokensDetails":[{"modality":"IMAGE","tokenCount":9223372036854775807},{"modality":"VIDEO","tokenCount":1}]}}`
	if _, err := decodeGemini([]byte(overflow), "gemini", false); err == nil {
		t.Fatal("overflowing image detail decoded without an error")
	}
}

func assertUsageJSON(t *testing.T, usage *wire.Usage, want string) {
	t.Helper()
	got, err := json.Marshal(usage)
	if err != nil || string(got) != want {
		t.Fatalf("usage = %s, want %s: %v", got, want, err)
	}
}

func TestAnthropicUsageAccounting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, usage, want string }{
		{
			"cache reads, tiered writes and server tools",
			`{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":6,"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":2},"server_tool_use":{"web_search_requests":2,"web_fetch_requests":0}}`,
			`{"input_tokens":19,"output_tokens":5,"total_tokens":24,"cached_input_tokens":3,"cache_write_input_tokens":6,"cache_write_5m_input_tokens":4,"cache_write_1h_input_tokens":2,"tool_calls":{"web_search":2}}`,
		},
		{"null cache categories", `{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":null,"cache_creation_input_tokens":null,"cache_creation":null,"server_tool_use":null}`, `{"input_tokens":10,"output_tokens":5,"total_tokens":15}`},
		{"explicit zeros", `{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0}`, `{"input_tokens":0,"output_tokens":0,"total_tokens":0,"cached_input_tokens":0}`},
		{"output not reported", `{"input_tokens":10}`, `null`},
		{"input not reported", `{"output_tokens":5}`, `null`},
		{"negative input", `{"input_tokens":-1,"output_tokens":5}`, ""},
		{"fractional output", `{"input_tokens":10,"output_tokens":5.0}`, ""},
		{"text count", `{"input_tokens":"10","output_tokens":5}`, ""},
		{"total overflow", `{"input_tokens":9223372036854775807,"output_tokens":1}`, ""},
		{"cache write detail beyond total", `{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":2}}`, ""},
		{"cache write detail without total", `{"input_tokens":10,"output_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":2}}`, ""},
		{"negative server tool count", `{"input_tokens":10,"output_tokens":5,"server_tool_use":{"web_search_requests":-1}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":` + tc.usage + `}`
			c, err := Decode(wire.Anthropic, wire.Anthropic, []byte(body), "route", "")
			if tc.want == "" {
				if err == nil {
					t.Fatalf("accepted %s", tc.usage)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertUsageJSON(t, c.Usage, tc.want)
		})
	}
}

func TestAnthropicStreamUsageTakesTheLatestCounts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, delta, want string }{
		{"output only", `{"output_tokens":9}`, `{"input_tokens":13,"output_tokens":9,"total_tokens":22,"cached_input_tokens":3}`},
		{"restated input", `{"input_tokens":20,"output_tokens":9,"cache_read_input_tokens":4}`, `{"input_tokens":24,"output_tokens":9,"total_tokens":33,"cached_input_tokens":4}`},
		{"output omitted", `{}`, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":3}}}` +
				"\n\nevent: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + tc.delta + `}` +
				"\n\nevent: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
			c, err := Stream(wire.Anthropic, wire.Anthropic, strings.NewReader(source), 4096, "route", func([]byte) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			assertUsageJSON(t, c.Usage, tc.want)
		})
	}
}

func TestGeminiUsageAccounting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, usage, want string }{
		{
			"thoughts are output and the provider total is kept",
			`{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":3,"cachedContentTokenCount":4,"totalTokenCount":21}`,
			`{"input_tokens":10,"output_tokens":8,"total_tokens":21,"cached_input_tokens":4,"reasoning_tokens":3}`,
		},
		{"total not reported", `{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":3}`, `{"input_tokens":10,"output_tokens":8,"total_tokens":18,"reasoning_tokens":3}`},
		{"blocked prompt has no candidates count", `{"promptTokenCount":10,"totalTokenCount":10}`, `{"input_tokens":10,"output_tokens":0,"total_tokens":10}`},
		{"prompt not reported", `{"candidatesTokenCount":5}`, `null`},
		{"negative candidates", `{"promptTokenCount":10,"candidatesTokenCount":-1}`, ""},
		{"text total", `{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":"15"}`, ""},
		{"negative modality count", `{"promptTokenCount":10,"candidatesTokenCount":5,"promptTokensDetails":[{"modality":"AUDIO","tokenCount":-1}]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":` + tc.usage + `}`
			c, err := Decode(wire.Gemini, wire.Gemini, []byte(body), "route", "")
			if tc.want == "" {
				if err == nil {
					t.Fatalf("accepted %s", tc.usage)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertUsageJSON(t, c.Usage, tc.want)
		})
	}
}
