package estimate

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"

	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
)

func TestBreakdownReconcilesWithContextInput(t *testing.T) {
	for _, length := range []int{4, 10_000} {
		request := mustParse(t, wire.Chat, fmt.Sprintf(`{
			"model":"fixture",
			"messages":[{"role":"developer","content":"Use the tool."},
				{"role":"user","content":[{"type":"text","text":"What is shown?"},{"type":"image_url","image_url":{"url":"https://example.test/image"}}]},
				{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"A tool result"}],
			"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","description":%q}}},
				{"type":"function","function":{"name":"search","parameters":{"type":"object","description":%q}}}]
		}`, strings.Repeat("catalog entry ", length), strings.Repeat("query filter ", length)))
		prompt := WalkWithBreakdown(request)
		for _, model := range []string{"gpt-5", "claude", "unknown-model"} {
			counter := ForModel(model, nil)
			result := prompt.Estimate(counter, nil)
			normal := Walk(request).Estimate(counter, nil)
			if result.ContextInput != normal.ContextInput || result.Input != normal.Input || result.Provenance != normal.Provenance {
				t.Fatalf("%s breakdown changed original token estimate: %+v vs %+v", model, result, normal)
			}
			b := result.Breakdown
			if b == nil || b.Instructions <= 0 || b.Conversation <= 0 || b.ToolCallsResults <= 0 ||
				b.Media != ImageTokens || b.ToolSchemas <= 0 || b.Unclassified < 0 ||
				b.Instructions+b.Conversation+b.ToolSchemas+b.ToolCallsResults+b.Media+b.Unclassified != result.ContextInput {
				t.Fatalf("%s categories do not explain %d tokens: %+v", model, result.ContextInput, b)
			}
			if length > 4 && b.ToolSchemas <= result.ContextInput/2 {
				t.Fatalf("%s missed the dominant schemas in a long request: %+v", model, b)
			}
		}
	}
}

func TestCompletionBreakdownShowsLargestPromptNotBatchTotal(t *testing.T) {
	request := mustParse(t, wire.Completions, `{"model":"fixture","prompt":[[1,2],[3,4,5]],"n":2,"max_tokens":10}`)
	result := WalkWithBreakdown(request).Estimate(ForModel("fixture", nil), nil)
	if result.Input != 5 || result.ContextInput != 3 || result.Breakdown == nil || *result.Breakdown != (Breakdown{Conversation: 3}) {
		t.Fatalf("batch breakdown should match the largest prompt: %+v, breakdown %+v", result, result.Breakdown)
	}
}

func mustParse(t testing.TB, family wire.Format, body string) *wire.Request {
	t.Helper()
	request, err := openai.Parse(family, []byte(body))
	if err != nil {
		t.Fatalf("parse %s: %v", body, err)
	}
	return request
}
