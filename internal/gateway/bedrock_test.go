package gateway

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

func bedrockMetadata(payload string) *eventstream.Message {
	h := eventstream.Headers{}
	h.Set(":message-type", eventstream.StringValue("event"))
	h.Set(":event-type", eventstream.StringValue("metadata"))
	return &eventstream.Message{Headers: h, Payload: []byte(payload)}
}

func TestBedrockStreamUsageAddsCacheTokensToInput(t *testing.T) {
	u := bedrockStreamUsage(bedrockMetadata(`{"usage":{"inputTokens":4,"outputTokens":1,"cacheReadInputTokens":1000,"cacheWriteInputTokens":20}}`))
	if u == nil {
		t.Fatal("cache reads beyond uncached input lost the usage")
	}
	if u.InputTokens != 1024 || u.OutputTokens != 1 ||
		u.CachedInputTokens == nil || *u.CachedInputTokens != 1000 ||
		u.CacheWriteInputTokens == nil || *u.CacheWriteInputTokens != 20 {
		t.Fatalf("usage = %+v", u)
	}
	for _, payload := range []string{
		`{"usage":{"inputTokens":4,"outputTokens":1,"cacheReadInputTokens":-1}}`,
		`{"usage":{"inputTokens":9223372036854775800,"outputTokens":1,"cacheReadInputTokens":20}}`,
	} {
		if u := bedrockStreamUsage(bedrockMetadata(payload)); u != nil {
			t.Fatalf("accepted %s: %+v", payload, u)
		}
	}
}
