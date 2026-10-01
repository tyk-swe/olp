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
	u, _ := bedrockStreamUsage(bedrockMetadata(`{"usage":{"inputTokens":4,"outputTokens":1,"cacheReadInputTokens":1000,"cacheWriteInputTokens":20}}`))
	if u == nil {
		t.Fatal("cache reads beyond uncached input lost the usage")
	}
	if u.InputTokens != 1024 || u.OutputTokens != 1 ||
		u.CachedInputTokens == nil || *u.CachedInputTokens != 1000 ||
		u.CacheWriteInputTokens == nil || *u.CacheWriteInputTokens != 20 {
		t.Fatalf("usage = %+v", u)
	}
	if u, _ := bedrockStreamUsage(bedrockMetadata(`{"usage":{"inputTokens":4,"outputTokens":1,"cacheReadInputTokens":-1}}`)); u != nil {
		t.Fatalf("accepted negative cache read: %+v", u)
	}
}
