package gateway

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func bedrockEvent(t *testing.T, kind string, payload any) *eventstream.Message {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	h := eventstream.Headers{}
	h.Set(":message-type", eventstream.StringValue("event"))
	h.Set(":event-type", eventstream.StringValue(kind))
	return &eventstream.Message{Headers: h, Payload: raw}
}

func bedrockChunk(t *testing.T, inner string) *eventstream.Message {
	t.Helper()
	return bedrockEvent(t, "chunk", map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(inner))})
}

func foldBedrockUsage(t *testing.T, messages ...*eventstream.Message) *openai.Usage {
	t.Helper()
	var usage *openai.Usage
	for _, message := range messages {
		if u, authoritative := bedrockStreamUsage(message); u != nil {
			if authoritative {
				usage = u
			} else {
				usage = mergeBedrockUsage(usage, u)
			}
		}
	}
	return usage
}

func TestBedrockInvokeStreamUsage(t *testing.T) {
	start := bedrockChunk(t, `{"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":1}}}`)
	delta := bedrockChunk(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)
	end := bedrockChunk(t, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`)
	stop := bedrockChunk(t, `{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":11,"outputTokenCount":7,"invocationLatency":1,"firstByteLatency":1}}`)

	u := foldBedrockUsage(t, start, delta, end, stop)
	if u == nil || u.InputTokens != 11 || u.OutputTokens != 7 || u.TotalTokens != 18 {
		t.Fatalf("invoke stream usage with metrics: %+v", u)
	}
	u = foldBedrockUsage(t, start, delta, end)
	if u == nil || u.InputTokens != 11 || u.OutputTokens != 7 || u.TotalTokens != 18 {
		t.Fatalf("message_delta must keep message_start input: %+v", u)
	}
	cached := foldBedrockUsage(t,
		bedrockChunk(t, `{"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":1,"cache_read_input_tokens":20,"cache_creation_input_tokens":30}}}`),
		bedrockChunk(t, `{"type":"message_delta","usage":{"output_tokens":5}}`))
	if cached == nil || cached.InputTokens != 53 || cached.OutputTokens != 5 || *cached.CachedInputTokens != 20 || *cached.CacheWriteInputTokens != 30 {
		t.Fatalf("invoke stream cache usage: %+v", cached)
	}
	if u, _ := bedrockStreamUsage(bedrockChunk(t, `{"type":"message_delta","usage":{"output_tokens":-1}}`)); u != nil {
		t.Fatalf("accepted negative usage: %+v", u)
	}
}

func TestBedrockConverseStreamCacheUsage(t *testing.T) {
	u, authoritative := bedrockStreamUsage(bedrockEvent(t, "metadata", map[string]any{"usage": map[string]any{
		"inputTokens": 12, "outputTokens": 200, "totalTokens": 4212, "cacheReadInputTokens": 4000,
	}}))
	if u == nil || !authoritative || u.InputTokens != 4012 || *u.CachedInputTokens != 4000 || u.TotalTokens != 4212 {
		t.Fatalf("cached Converse stream usage: %+v", u)
	}
	if u, _ := bedrockStreamUsage(bedrockEvent(t, "metadata", map[string]any{"usage": map[string]any{
		"inputTokens": 12, "outputTokens": 2, "cacheReadInputTokens": -1,
	}})); u != nil {
		t.Fatalf("accepted negative cache usage: %+v", u)
	}
}
