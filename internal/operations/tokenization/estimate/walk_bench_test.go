package estimate

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// benchRequest is a chat request of the size the benchmark names: a system
// message, a long user message, and a tool catalogue.
func benchRequest(tb testing.TB, tokens int) *openai.Request {
	tb.Helper()
	text := promptOf(tb, O200kBase, corpus(tb, "prose-2", "prose-3", "prose-6"), tokens)
	body, err := json.Marshal(map[string]any{
		"model": "m", "max_tokens": 16,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a careful assistant."},
			map[string]any{"role": "user", "content": text},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "description": "Look it up", "parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}}}},
	})
	if err != nil {
		tb.Fatal(err)
	}
	request, err := openai.Parse(openai.FamilyChat, body)
	if err != nil {
		tb.Fatal(err)
	}
	return request
}

// BenchmarkWalkAgainstLegacy prices the walk and a heuristic count of it next
// to the walker this package replaced, which is what every request paid before
// any family had a tokenizer. The walk keeps the first retainBytes of the text
// it reads, so that a family counted later has all of it that a counter reads.
func BenchmarkWalkAgainstLegacy(b *testing.B) {
	for _, size := range []struct {
		name   string
		tokens int
	}{{"small", 30}, {"4k", 4000}, {"100k", 100000}} {
		request := benchRequest(b, size.tokens)
		b.Run(size.name+"/legacy", func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				legacyEstimateTokens(request)
			}
		})
		b.Run(size.name+"/walk+heuristic", func(b *testing.B) {
			b.ReportAllocs()
			counter := ForModel("claude-sonnet-4-5")
			for range b.N {
				Walk(request).Estimate(counter, nil)
			}
		})
		b.Run(size.name+"/walk+o200k", func(b *testing.B) {
			b.ReportAllocs()
			counter := ForModel("gpt-4o")
			for range b.N {
				Walk(request).Estimate(counter, nil)
			}
		})
		b.Run(size.name+"/reuse", func(b *testing.B) {
			b.ReportAllocs()
			counter := ForModel("gpt-4o")
			prompt := Walk(request)
			defaults := map[string]json.RawMessage{"max_tokens": json.RawMessage(`100`)}
			for range b.N {
				prompt.Estimate(counter, defaults)
			}
		})
	}
}
