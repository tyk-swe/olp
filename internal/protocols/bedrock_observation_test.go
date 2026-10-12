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

func TestBedrockObservationUsagePresenceAndNativeCacheSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, kind, document                       string
		input, output, total                       int64
		inputPresent, outputPresent, authoritative bool
		short, long                                int64
	}{
		{"partial metadata", "metadata", `{"usage":{"inputTokens":3}}`, 3, 0, 3, true, false, false, 0, 0},
		{"explicit zero metadata", "metadata", `{"usage":{"inputTokens":0,"outputTokens":0}}`, 0, 0, 0, true, true, true, 0, 0},
		{"native total and cache categories", "metadata", `{"usage":{"inputTokens":100,"outputTokens":2,"totalTokens":102,"cacheReadInputTokens":20,"cacheWriteInputTokens":30,"cacheDetails":[{"ttl":"5m","inputTokens":10},{"ttl":"1h","inputTokens":5}]}}`, 150, 2, 102, true, true, true, 10, 5},
		{"Anthropic cache categories", "chunk", `{"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":2,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":5}}}}`, 53, 2, 55, true, true, false, 10, 5},
		{"partial invocation metrics", "chunk", `{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":3}}`, 3, 0, 3, true, false, false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := observedBedrockMessage(t, tc.kind, tc.document)
			original := string(message.Payload)
			observation, err := ObserveBedrockEvent(message, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			u := observation.Usage.Value
			if u == nil || u.InputTokens != tc.input || u.OutputTokens != tc.output || u.TotalTokens != tc.total || observation.Usage.InputPresent != tc.inputPresent || observation.Usage.OutputPresent != tc.outputPresent || observation.Usage.Authoritative != tc.authoritative {
				t.Fatalf("wrong typed observation: %+v %+v", observation.Usage, u)
			}
			if tc.short > 0 && (u.CacheWrite5MInputTokens == nil || *u.CacheWrite5MInputTokens != tc.short || u.CacheWrite1HInputTokens == nil || *u.CacheWrite1HInputTokens != tc.long) {
				t.Fatalf("lost cache categories: %+v", u)
			}
			if string(message.Payload) != original {
				t.Fatal("observation rewrote native bytes")
			}
		})
	}
	for _, document := range []string{`{}`, `{"usage":{}}`, `{"usage":null}`} {
		observation, err := ObserveBedrockEvent(observedBedrockMessage(t, "metadata", document), 1<<20)
		if err != nil || observation.Usage.Value != nil || observation.Usage.Authoritative {
			t.Fatalf("absent usage became zero: %+v %v", observation, err)
		}
	}
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

func TestBedrockObservationMetricsRetainMatchingNativeCacheDetails(t *testing.T) {
	start, err := ObserveBedrockEvent(observedBedrockMessage(t, "chunk", `{"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":2,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":5}}}}`), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := ObserveBedrockEvent(observedBedrockMessage(t, "chunk", `{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":3,"outputTokenCount":7,"cacheReadInputTokenCount":20,"cacheWriteInputTokenCount":30}}`), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	u, err := stop.Usage.Merge(start.Usage.Value)
	if err != nil || u == nil || u.InputTokens != 53 || u.OutputTokens != 7 || u.CacheWrite5MInputTokens == nil || *u.CacheWrite5MInputTokens != 10 || u.CacheWrite1HInputTokens == nil || *u.CacheWrite1HInputTokens != 5 {
		t.Fatalf("matching metrics erased cache categories: %+v %v", u, err)
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
