package protocols

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
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

func mergedBedrockUsage(t *testing.T, kind string, events []string) (*wire.Usage, bool) {
	t.Helper()
	if len(events) == 0 {
		return nil, false
	}
	previous, _ := mergedBedrockUsage(t, kind, events[:len(events)-1])
	observed, err := ObserveBedrockEvent(observedBedrockMessage(t, kind, events[len(events)-1]), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := observed.Usage.Merge(previous)
	if err != nil {
		t.Fatal(err)
	}
	return merged, observed.Usage.Authoritative
}

func TestBedrockObservedUsageMergesAcrossEvents(t *testing.T) {
	t.Parallel()
	const start = `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":3,"cache_creation_input_tokens":6,"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":2}}}}`
	const delta = `{"type":"message_delta","usage":{"output_tokens":9}}`
	const merged = `{"input_tokens":19,"output_tokens":9,"total_tokens":28,"cached_input_tokens":3,"cache_write_input_tokens":6,"cache_write_5m_input_tokens":4,"cache_write_1h_input_tokens":2}`
	for _, tc := range []struct {
		name, kind    string
		events        []string
		want          string
		authoritative bool
	}{
		{"delta updates only the output", "chunk", []string{start, delta}, merged, false},
		{"invocation metrics replace totals and keep cache detail", "chunk", []string{start, delta, `{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":10,"outputTokenCount":12,"cacheReadInputTokenCount":3,"cacheWriteInputTokenCount":6}}`}, `{"input_tokens":19,"output_tokens":12,"total_tokens":31,"cached_input_tokens":3,"cache_write_input_tokens":6,"cache_write_5m_input_tokens":4,"cache_write_1h_input_tokens":2}`, true},
		{"Converse metadata", "metadata", []string{`{"usage":{"inputTokens":10,"outputTokens":5,"totalTokens":99,"cacheReadInputTokens":3,"cacheWriteInputTokens":2,"cacheDetails":[{"ttl":"5m","inputTokens":2}]}}`}, `{"input_tokens":15,"output_tokens":5,"total_tokens":99,"cached_input_tokens":3,"cache_write_input_tokens":2,"cache_write_5m_input_tokens":2}`, true},
		{"partial Converse metadata", "metadata", []string{`{"usage":{"outputTokens":5}}`}, `{"input_tokens":0,"output_tokens":5,"total_tokens":5}`, false},
		{"no usage", "metadata", []string{`{"usage":null}`}, `null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage, authoritative := mergedBedrockUsage(t, tc.kind, tc.events)
			assertUsageJSON(t, usage, tc.want)
			if authoritative != tc.authoritative {
				t.Fatalf("authoritative = %t, want %t", authoritative, tc.authoritative)
			}
		})
	}
}
