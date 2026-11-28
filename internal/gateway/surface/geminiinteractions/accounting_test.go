package geminiinteractions

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/oif"
)

func TestInteractionUsageAccounting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, object, want string }{
		{"totals", `{"usage":{"total_input_tokens":10,"total_output_tokens":5,"total_cached_tokens":4,"total_tokens":15}}`,
			`{"input_tokens":10,"output_tokens":5,"total_tokens":15,"cached_input_tokens":4}`},
		{"tool use and thoughts are billed", `{"usage":{"total_input_tokens":10,"total_output_tokens":5,"total_tool_use_tokens":3,"total_thought_tokens":2,"total_tokens":15}}`,
			`{"input_tokens":13,"output_tokens":7,"total_tokens":20,"reasoning_tokens":2}`},
		{"larger reported total", `{"usage":{"total_input_tokens":10,"total_output_tokens":5,"total_tokens":18}}`,
			`{"input_tokens":10,"output_tokens":5,"total_tokens":18}`},
		{"no usage", `{"status":"completed"}`, `null`},
		{"output missing", `{"usage":{"total_input_tokens":10}}`, `null`},
		{"negative output", `{"usage":{"total_input_tokens":10,"total_output_tokens":-1}}`, `null`},
		{"negative thoughts", `{"usage":{"total_input_tokens":10,"total_output_tokens":5,"total_thought_tokens":-1}}`, `null`},
		{"cached above input", `{"usage":{"total_input_tokens":10,"total_output_tokens":5,"total_cached_tokens":11}}`, `null`},
		{"fractional count", `{"usage":{"total_input_tokens":10.5,"total_output_tokens":5}}`, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc, err := oif.ParseJSON([]byte(tc.object), oif.Limits{MaxBytes: 1 << 12, MaxDepth: 8, MaxNodes: 64})
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(interactionUsage(doc.Root()))
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != tc.want {
				t.Fatalf("usage = %s, want %s", encoded, tc.want)
			}
		})
	}
}
