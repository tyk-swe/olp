package protocols

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

func observedBedrockMessage(t *testing.T, kind, document string) *eventstream.Message {
	t.Helper()
	headers := eventstream.Headers{}
	headers.Set(":message-type", eventstream.StringValue("event"))
	headers.Set(":event-type", eventstream.StringValue(kind))
	payload := []byte(document)
	if kind == "chunk" {
		payload, _ = json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString(payload)})
	}
	return &eventstream.Message{Headers: headers, Payload: payload}
}

func TestBedrockObservationRejectsMalformedUsage(t *testing.T) {
	for _, usage := range []string{
		`{"inputTokens":-1,"outputTokens":2}`,
		`{"inputTokens":1.5,"outputTokens":2}`,
		`{"inputTokens":"1","outputTokens":2}`,
		`{"inputTokens":null,"outputTokens":2}`,
		`{"inputTokens":9223372036854775808,"outputTokens":2}`,
		`{"inputTokens":9223372036854775807,"outputTokens":1}`,
		`{"inputTokens":9223372036854775800,"outputTokens":0,"cacheReadInputTokens":20}`,
		`{"inputTokens":1,"outputTokens":2,"totalTokens":-1}`,
		`{"inputTokens":1,"outputTokens":2,"cacheDetails":[{"ttl":"5m","inputTokens":0}]}`,
		`{"inputTokens":1,"outputTokens":2,"cacheWriteInputTokens":30,"cacheDetails":[{"ttl":"5m","inputTokens":10},{"ttl":"5m","inputTokens":5}]}`,
		`{"inputTokens":1,"outputTokens":2,"cacheWriteInputTokens":30,"cacheDetails":[{"ttl":"10m","inputTokens":10}]}`,
	} {
		if _, err := ObserveBedrockEvent(observedBedrockMessage(t, "metadata", `{"usage":`+usage+`}`), 1<<20); err == nil {
			t.Fatalf("accepted malformed usage %s", usage)
		}
	}
	for _, usage := range []string{
		`{"input_tokens":3,"output_tokens":2,"cache_creation":{"ephemeral_5m_input_tokens":0}}`,
		`{"input_tokens":3,"output_tokens":2,"cache_creation_input_tokens":10,"cache_creation":{"ephemeral_5m_input_tokens":7,"ephemeral_1h_input_tokens":5}}`,
		`{"input_tokens":3,"output_tokens":2,"cache_creation_input_tokens":10,"cache_creation":{"ephemeral_5m_input_tokens":9223372036854775807,"ephemeral_1h_input_tokens":9223372036854775807}}`,
	} {
		if _, err := ObserveBedrockEvent(observedBedrockMessage(t, "chunk", `{"type":"message_start","message":{"usage":`+usage+`}}`), 1<<20); err == nil {
			t.Fatalf("accepted malformed Anthropic usage %s", usage)
		}
	}
}

func TestBedrockObservationMetricsRetainOmittedCacheCategories(t *testing.T) {
	start, err := ObserveBedrockEvent(observedBedrockMessage(t, "chunk", `{"type":"message_start","message":{"usage":{"input_tokens":3,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":12,"ephemeral_1h_input_tokens":18}}}}`), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := ObserveBedrockEvent(observedBedrockMessage(t, "chunk", `{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":53,"outputTokenCount":7}}`), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	u, err := stop.Usage.Merge(start.Usage.Value)
	if err != nil || u.InputTokens != 53 || u.OutputTokens != 7 || u.CachedInputTokens == nil || *u.CachedInputTokens != 20 || u.CacheWriteInputTokens == nil || *u.CacheWriteInputTokens != 30 || u.CacheWrite5MInputTokens == nil || *u.CacheWrite5MInputTokens != 12 || u.CacheWrite1HInputTokens == nil || *u.CacheWrite1HInputTokens != 18 {
		t.Fatalf("summary dropped native cache observations: %+v, %v", u, err)
	}
}
