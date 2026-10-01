package protocols

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func anthropicBody(usage string) []byte {
	return []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"wire-model","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":` + usage + `}`)
}

func TestAnthropicCacheWriteUsage(t *testing.T) {
	usage := `{"input_tokens":3,"output_tokens":2,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":5}}`
	c, err := Decode(openai.FamilyAnthropic, openai.FamilyChat, anthropicBody(usage), "team-model", "")
	if err != nil {
		t.Fatal(err)
	}
	u := c.Usage
	if u == nil || u.InputTokens != 53 || u.OutputTokens != 2 || u.TotalTokens != 55 {
		t.Fatalf("usage totals: %+v", u)
	}
	if *u.CachedInputTokens != 20 || *u.CacheWriteInputTokens != 30 ||
		*u.CacheWrite5MInputTokens != 10 || *u.CacheWrite1HInputTokens != 5 {
		t.Fatalf("cache categories: %+v", u)
	}
	native, err := Decode(openai.FamilyAnthropic, openai.FamilyAnthropic, anthropicBody(usage), "team-model", "")
	if err != nil || !bytes.Contains(native.Body, []byte("ephemeral_5m_input_tokens")) {
		t.Fatalf("native cache detail lost: %s %v", native.Body, err)
	}
}

func TestAnthropicCacheWriteValidation(t *testing.T) {
	for _, tc := range []struct{ name, usage string }{
		{"detail beyond generic write", `{"input_tokens":3,"output_tokens":2,"cache_creation_input_tokens":10,"cache_creation":{"ephemeral_5m_input_tokens":7,"ephemeral_1h_input_tokens":5}}`},
		{"negative write", `{"input_tokens":3,"output_tokens":2,"cache_creation_input_tokens":-1}`},
		{"noninteger detail", `{"input_tokens":3,"output_tokens":2,"cache_creation_input_tokens":10,"cache_creation":{"ephemeral_5m_input_tokens":1.5}}`},
		{"detail without generic write", `{"input_tokens":3,"output_tokens":2,"cache_creation":{"ephemeral_5m_input_tokens":4}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(openai.FamilyAnthropic, openai.FamilyChat, anthropicBody(tc.usage), "team-model", ""); err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
		})
	}
}

func TestAnthropicCacheWriteStreaming(t *testing.T) {
	var source strings.Builder
	frame := func(event string, v any) {
		source.WriteString("event: " + event + "\ndata: " + string(raw(v)) + "\n\n")
	}
	frame("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "m", "type": "message", "role": "assistant", "model": "wire-model", "content": []any{},
		"usage": map[string]any{"input_tokens": 3, "output_tokens": 0,
			"cache_read_input_tokens": 20, "cache_creation_input_tokens": 30,
			"cache_creation": map[string]any{"ephemeral_5m_input_tokens": 10}}}})
	frame("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	frame("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "hi"}})
	frame("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	frame("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 2}})
	frame("message_stop", map[string]any{"type": "message_stop"})
	c, err := Stream(openai.FamilyAnthropic, openai.FamilyChat, strings.NewReader(source.String()), 8192, "team-model", true, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	u := c.Usage
	if u == nil || u.InputTokens != 53 || u.OutputTokens != 2 ||
		*u.CachedInputTokens != 20 || *u.CacheWriteInputTokens != 30 ||
		*u.CacheWrite5MInputTokens != 10 || u.CacheWrite1HInputTokens != nil {
		t.Fatalf("streamed cache usage: %+v", u)
	}
}

func bedrockUsageValue(t *testing.T, usage string) *openai.Usage {
	t.Helper()
	u, err := bedrockUsage([]byte(usage))
	if err != nil {
		t.Fatalf("bedrockUsage: %v", err)
	}
	return u
}

func TestBedrockCacheUsage(t *testing.T) {
	u := bedrockUsageValue(t, `{"inputTokens":100,"outputTokens":2,"totalTokens":102,"cacheReadInputTokens":20,"cacheWriteInputTokens":30,"cacheDetails":[{"ttl":"5m","inputTokens":10},{"ttl":"1h","inputTokens":5}]}`)
	if u.InputTokens != 150 || u.OutputTokens != 2 || u.TotalTokens != 102 {
		t.Fatalf("Bedrock input must include cache reads and writes: %+v", u)
	}
	if *u.CachedInputTokens != 20 || *u.CacheWriteInputTokens != 30 ||
		*u.CacheWrite5MInputTokens != 10 || *u.CacheWrite1HInputTokens != 5 {
		t.Fatalf("cache categories: %+v", u)
	}
	// AWS's prompt-caching example reports the cache write beside inputTokens.
	u = bedrockUsageValue(t, `{"inputTokens":4,"outputTokens":1,"totalTokens":1153,"cacheReadInputTokens":0,"cacheWriteInputTokens":1148}`)
	if u.InputTokens != 1152 || *u.CacheWriteInputTokens != 1148 || u.TotalTokens != 1153 {
		t.Fatalf("cache write beyond uncached input: %+v", u)
	}
	u = bedrockUsageValue(t, `{"inputTokens":4,"outputTokens":1,"cacheReadInputTokens":1000}`)
	if u.InputTokens != 1004 || *u.CachedInputTokens != 1000 || u.TotalTokens != 1005 {
		t.Fatalf("derived total: %+v", u)
	}
	cached := bedrockUsageValue(t, `{"inputTokens":12,"outputTokens":200,"cacheReadInputTokens":4000}`)
	if cached.InputTokens != 4012 || *cached.CachedInputTokens != 4000 || cached.TotalTokens != 4212 {
		t.Fatalf("cache reads beyond uncached input: %+v", cached)
	}
}

func TestBedrockCacheUsageValidation(t *testing.T) {
	for _, tc := range []struct{ name, usage string }{
		{"unknown TTL", `{"inputTokens":100,"outputTokens":2,"cacheWriteInputTokens":30,"cacheDetails":[{"ttl":"10m","inputTokens":5}]}`},
		{"duplicate TTL", `{"inputTokens":100,"outputTokens":2,"cacheWriteInputTokens":30,"cacheDetails":[{"ttl":"5m","inputTokens":5},{"ttl":"5m","inputTokens":4}]}`},
		{"detail beyond generic write", `{"inputTokens":100,"outputTokens":2,"cacheWriteInputTokens":10,"cacheDetails":[{"ttl":"5m","inputTokens":8},{"ttl":"1h","inputTokens":5}]}`},
		{"input overflow", `{"inputTokens":9223372036854775800,"outputTokens":2,"cacheReadInputTokens":20}`},
		{"negative write", `{"inputTokens":100,"outputTokens":2,"cacheWriteInputTokens":-1}`},
		{"noninteger read", `{"inputTokens":100,"outputTokens":2,"cacheReadInputTokens":1.5}`},
		{"negative detail", `{"inputTokens":100,"outputTokens":2,"cacheWriteInputTokens":30,"cacheDetails":[{"ttl":"5m","inputTokens":-1}]}`},
		{"malformed detail", `{"inputTokens":100,"outputTokens":2,"cacheWriteInputTokens":30,"cacheDetails":["5m"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := bedrockUsage([]byte(tc.usage)); err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
		})
	}
}

func TestBedrockCacheUsageStreaming(t *testing.T) {
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
	add("contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]string{"text": "hi"}})
	add("contentBlockStop", map[string]int{"contentBlockIndex": 0})
	add("messageStop", map[string]string{"stopReason": "end_turn"})
	add("metadata", map[string]any{"usage": map[string]any{
		"inputTokens": 100, "outputTokens": 2, "cacheReadInputTokens": 20,
		"cacheWriteInputTokens": 30,
		"cacheDetails":          []any{map[string]any{"ttl": "5m", "inputTokens": 10}}}})
	c, err := Stream("bedrock", openai.FamilyChat, bytes.NewReader(encoded.Bytes()), 8192, "team-model", true, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	u := c.Usage
	if u == nil || u.InputTokens != 150 || *u.CachedInputTokens != 20 ||
		*u.CacheWriteInputTokens != 30 || *u.CacheWrite5MInputTokens != 10 {
		t.Fatalf("streamed cache usage: %+v", u)
	}

	var bad bytes.Buffer
	addBad := func(event string, value any) {
		t.Helper()
		h := eventstream.Headers{}
		h.Set(":message-type", eventstream.StringValue("event"))
		h.Set(":event-type", eventstream.StringValue(event))
		if err := eventstream.NewEncoder().Encode(&bad, eventstream.Message{Headers: h, Payload: raw(value)}); err != nil {
			t.Fatal(err)
		}
	}
	addBad("messageStart", map[string]any{"role": "assistant"})
	addBad("messageStop", map[string]string{"stopReason": "end_turn"})
	addBad("metadata", map[string]any{"usage": map[string]any{
		"inputTokens": 100, "outputTokens": 2, "cacheWriteInputTokens": 30,
		"cacheDetails": []any{map[string]any{"ttl": "10m", "inputTokens": 10}}}})
	if _, err = Stream("bedrock", openai.FamilyChat, bytes.NewReader(bad.Bytes()), 8192, "team-model", true, func([]byte) error { return nil }); err == nil {
		t.Fatal("accepted an unknown cache TTL on a stream")
	}
}
