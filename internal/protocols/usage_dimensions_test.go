package protocols

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

var openaiEmit = func([]byte) error { return nil }
var _ = openai.FamilyChat

func TestAnthropicUsageCountsServerToolRequests(t *testing.T) {
	body := `{"type":"message","role":"assistant","model":"claude","stop_reason":"end_turn",
		"content":[{"type":"text","text":"hi"}],
		"usage":{"input_tokens":10,"output_tokens":5,
			"server_tool_use":{"web_search_requests":3,"web_fetch_requests":2,"code_execution_requests":9}}}`
	c, err := decodeAnthropic([]byte(body), "claude")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"web_search": 3, "web_fetch": 2}
	if !reflect.DeepEqual(c.Usage.ToolCalls, want) {
		t.Fatalf("tool calls %v, want %v", c.Usage.ToolCalls, want)
	}
}

func TestAnthropicStreamCountsServerToolRequests(t *testing.T) {
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":10,"output_tokens":1,"server_tool_use":{"web_search_requests":1}}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4,"server_tool_use":{"web_search_requests":2,"web_fetch_requests":1}}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	c, err := streamAnthropicEvents(strings.NewReader(stream), 1<<20, "claude", openaiEmit, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"web_search": 2, "web_fetch": 1}
	if !reflect.DeepEqual(c.Usage.ToolCalls, want) {
		t.Fatalf("tool calls %v, want %v", c.Usage.ToolCalls, want)
	}
}

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
}

func TestGeminiStreamAccumulatesGroundingAcrossChunks(t *testing.T) {
	chunk := func(index int, queries string) string {
		candidate := `{"index":` + strconv.Itoa(index) + `,"content":{"role":"model","parts":[{"text":"x"}]},"groundingMetadata":{"webSearchQueries":[` + queries + `]},"finishReason":"STOP"`
		return `data: {"candidates":[` + candidate + `}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}` + "\n\n"
	}
	stream := chunk(0, `"a"`) + chunk(1, `"b","c"`)
	c, err := streamGeminiEvents(strings.NewReader(stream), 1<<20, "gemini", openaiEmit, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"google_search_query": 3, "google_search_prompt": 1}
	if !reflect.DeepEqual(c.Usage.ToolCalls, want) {
		t.Fatalf("tool calls %v, want %v", c.Usage.ToolCalls, want)
	}
}
