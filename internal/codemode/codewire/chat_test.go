package codewire_test

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codemode/codewire"
)

func assertUsage(t *testing.T, usage codemode.Usage, want string) {
	t.Helper()
	if got, err := json.Marshal(usage); err != nil || string(got) != want {
		t.Fatalf("usage = %s, want %s: %v", got, want, err)
	}
}

const noUsage = `{"Total":null,"Input":null,"Output":null,"Cached":null,"Reasoning":null}`

func TestChatUsageEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, usage, want string }{
		{
			"details",
			`{"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500,"prompt_tokens_details":{"cached_tokens":1000},"completion_tokens_details":{"reasoning_tokens":100}}`,
			`{"Total":1500,"Input":1200,"Output":300,"Cached":1000,"Reasoning":100}`,
		},
		{"total not reported", `{"prompt_tokens":11,"completion_tokens":7}`, `{"Total":18,"Input":11,"Output":7,"Cached":null,"Reasoning":null}`},
		{"null total stays unknown", `{"prompt_tokens":11,"completion_tokens":7,"total_tokens":null}`, `{"Total":null,"Input":11,"Output":7,"Cached":null,"Reasoning":null}`},
		{"output not reported", `{"prompt_tokens":11}`, `{"Total":null,"Input":11,"Output":null,"Cached":null,"Reasoning":null}`},
		{"largest exact count", `{"prompt_tokens":9007199254740991}`, `{"Total":null,"Input":9007199254740991,"Output":null,"Cached":null,"Reasoning":null}`},
		{"unusable counts are unknown", `{"prompt_tokens":9007199254740992,"completion_tokens":-1,"total_tokens":"5","prompt_tokens_details":{"cached_tokens":1.0}}`, noUsage},
		{"inconsistent total", `{"prompt_tokens":11,"completion_tokens":7,"total_tokens":99}`, noUsage},
		{"cached beyond input", `{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":12}}`, noUsage},
		{"null usage", `null`, noUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := codewire.NewChat(1).Unary([]byte(`{"choices":[{"index":0,"finish_reason":"stop"}],"usage":` + tc.usage + `}`))
			assertUsage(t, got.Usage, tc.want)
		})
	}
}

func TestChatStreamKeepsTheLastReportedUsage(t *testing.T) {
	t.Parallel()
	chat := codewire.NewChat(1)
	chat.Event([]byte(`{"choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":11,"completion_tokens":1}}`))
	chat.Event([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`))
	chat.Event([]byte(`{"choices":[],"usage":null}`))
	assertUsage(t, chat.Event([]byte(`[DONE]`)).Usage, `{"Total":18,"Input":11,"Output":7,"Cached":null,"Reasoning":null}`)
}
