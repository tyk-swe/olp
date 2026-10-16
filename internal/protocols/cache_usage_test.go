package protocols

import (
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func anthropicBody(usage string) []byte {
	return []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"wire-model","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":` + usage + `}`)
}

func bedrockUsageValue(t *testing.T, usage string) *wire.Usage {
	t.Helper()
	u, err := bedrockUsage([]byte(usage))
	if err != nil {
		t.Fatalf("bedrockUsage: %v", err)
	}
	return u
}

// AWS's prompt-caching example reports the cache write beside inputTokens.

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
		{"zero detail without generic write", `{"inputTokens":100,"outputTokens":2,"cacheDetails":[{"ttl":"5m","inputTokens":0}]}`},
		{"read plus write overflow", `{"inputTokens":10,"outputTokens":2,"cacheReadInputTokens":9223372036854775807,"cacheWriteInputTokens":9223372036854775807}`},
		{"detail sum overflow", `{"inputTokens":10,"outputTokens":2,"cacheWriteInputTokens":10,"cacheDetails":[{"ttl":"5m","inputTokens":9223372036854775807},{"ttl":"1h","inputTokens":9223372036854775807}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := bedrockUsage([]byte(tc.usage)); err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
		})
	}
}
