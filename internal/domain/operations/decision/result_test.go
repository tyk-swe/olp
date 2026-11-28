package decision

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations"
)

func TestDecisionUsageMustAddUp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, usage, want string }{
		{"computed total", `{"input_tokens":10,"output_tokens":5}`, `{"input":10,"output":5,"total":15}`},
		{"cache and reasoning", `{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":6,"cache_write_tokens":4},"output_tokens_details":{"reasoning_tokens":5}}`, `{"input":10,"output":5,"total":15,"cached":6,"written":4,"reasoning":5}`},
		{"output missing", `{"input_tokens":10}`, ""},
		{"negative input", `{"input_tokens":-1,"output_tokens":5}`, ""},
		{"total above sum", `{"input_tokens":10,"output_tokens":5,"total_tokens":16}`, ""},
		{"total below sum", `{"input_tokens":10,"output_tokens":5,"total_tokens":14}`, ""},
		{"sum overflows", `{"input_tokens":9223372036854775807,"output_tokens":1}`, ""},
		{"cached above input", `{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":11}}`, ""},
		{"cache categories above input", `{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":6,"cache_write_tokens":5}}`, ""},
		{"reasoning above output", `{"input_tokens":10,"output_tokens":5,"output_tokens_details":{"reasoning_tokens":6}}`, ""},
		{"unknown category", `{"input_tokens":10,"output_tokens":5,"image_tokens":1}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc, err := oif.ParseJSON([]byte(tc.usage), oif.Limits{MaxBytes: 1 << 12, MaxDepth: 8, MaxNodes: 64})
			if err != nil {
				t.Fatal(err)
			}
			usage, ok := parseDecisionUsage(doc.Root())
			if !ok {
				if tc.want != "" {
					t.Fatal("usage was rejected")
				}
				return
			}
			encoded, err := json.Marshal(struct {
				Input     *int64 `json:"input,omitempty"`
				Output    *int64 `json:"output,omitempty"`
				Total     *int64 `json:"total,omitempty"`
				Cached    *int64 `json:"cached,omitempty"`
				Written   *int64 `json:"written,omitempty"`
				Reasoning *int64 `json:"reasoning,omitempty"`
			}{usage.InputTokens, usage.OutputTokens, usage.TotalTokens, usage.CachedInputTokens, usage.CacheWriteInputTokens, usage.ReasoningTokens})
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != tc.want {
				t.Fatalf("usage = %s, want %q", encoded, tc.want)
			}
		})
	}
}

func TestGeminiDecisionUsageBillsInputOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, response string
		allowMissing   bool
		want           string
	}{
		{"input only", `{"usage":{"total_input_tokens":9,"total_output_tokens":0,"total_tokens":9}}`, false, "input 9 output 0 total 9"},
		{"missing when allowed", `{}`, true, "unobserved"},
		{"missing when required", `{}`, false, "rejected"},
		{"output tokens", `{"usage":{"total_input_tokens":9,"total_output_tokens":1,"total_tokens":10}}`, false, "rejected"},
		{"total differs from input", `{"usage":{"total_input_tokens":9,"total_output_tokens":0,"total_tokens":10}}`, false, "rejected"},
		{"negative input", `{"usage":{"total_input_tokens":-1,"total_output_tokens":0,"total_tokens":-1}}`, false, "rejected"},
		{"total missing", `{"usage":{"total_input_tokens":9,"total_output_tokens":0}}`, true, "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc, err := oif.ParseJSON([]byte(tc.response), oif.Limits{MaxBytes: 1 << 12, MaxDepth: 8, MaxNodes: 64})
			if err != nil {
				t.Fatal(err)
			}
			usage, err := geminiUsage(member(doc.Root(), "usage"), tc.allowMissing)
			if got := describeGeminiUsage(usage, err); got != tc.want {
				t.Fatalf("usage = %s, want %s: %v", got, tc.want, err)
			}
		})
	}
}

func describeGeminiUsage(usage *operations.Usage, err error) string {
	switch {
	case err != nil:
		return "rejected"
	case usage == nil:
		return "unobserved"
	}
	return fmt.Sprintf("input %d output %d total %d", *usage.InputTokens, *usage.OutputTokens, *usage.TotalTokens)
}
