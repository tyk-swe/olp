package protocols

import (
	"testing"
)

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

func TestBedrockConverseUsageAccounting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, usage, want string }{
		{
			"cache categories are added to input",
			`{"inputTokens":10,"outputTokens":5,"totalTokens":24,"cacheReadInputTokens":3,"cacheWriteInputTokens":6,"cacheDetails":[{"ttl":"5m","inputTokens":4},{"ttl":"1h","inputTokens":2}]}`,
			`{"input_tokens":19,"output_tokens":5,"total_tokens":24,"cached_input_tokens":3,"cache_write_input_tokens":6,"cache_write_5m_input_tokens":4,"cache_write_1h_input_tokens":2}`,
		},
		{"provider total is kept", `{"inputTokens":10,"outputTokens":5,"totalTokens":99}`, `{"input_tokens":10,"output_tokens":5,"total_tokens":99}`},
		{"null optional counts", `{"inputTokens":10,"outputTokens":5,"totalTokens":null,"cacheReadInputTokens":null,"cacheWriteInputTokens":null,"cacheDetails":null}`, `{"input_tokens":10,"output_tokens":5,"total_tokens":15}`},
		{"unusable total is replaced by the sum", `{"inputTokens":10,"outputTokens":5,"totalTokens":-1}`, `{"input_tokens":10,"output_tokens":5,"total_tokens":15}`},
		{"output not reported", `{"inputTokens":10,"totalTokens":10}`, ""},
		{"input not reported", `{"outputTokens":5,"totalTokens":5}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage, err := bedrockUsage([]byte(tc.usage))
			if tc.want == "" {
				if err == nil {
					t.Fatalf("accepted %s", tc.usage)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertUsageJSON(t, usage, tc.want)
		})
	}
}
