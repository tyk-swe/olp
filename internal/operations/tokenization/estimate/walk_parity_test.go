package estimate

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// walkerFamilies is every request family the gateway can hand to admission.
// The walker must charge each as the old walker did, including the ones whose
// prompt it does not read.
var walkerFamilies = []openai.Family{
	openai.FamilyChat, openai.FamilyResponses, openai.FamilyInputTokens,
	openai.FamilyEmbeddings, openai.FamilyModeration, openai.FamilyRerank,
	openai.FamilyAnthropic, openai.FamilyAnthropicCount,
	openai.FamilyGemini, openai.FamilyGeminiStream, openai.FamilyGeminiCount,
	openai.FamilyGeminiInteractions, openai.FamilyGeminiEmbeddings,
	openai.FamilyBedrock, openai.FamilyBedrockEmbeddings, openai.FamilyBedrockInvoke,
	openai.FamilyVertexEmbeddings, openai.FamilyImageGeneration, openai.FamilySpeech,
}

// requestGen builds requests from a seeded source. Most of what it makes is a
// shape a real client sends; the rest is any JSON at all, because the walker
// must charge a field it does not understand nothing and must not fail on it.
type requestGen struct{ r *rand.Rand }

func (g requestGen) intn(n int) int { return g.r.IntN(n) }

var textPieces = []string{
	"hello", "world", "the quick brown fox", "日本語で", "\U0001f600", "café", " ", "  ", "\n", "\t",
	"x", "package main", "{\"a\":1}", "\u0000", "�", "é", "العربية", "12345",
	"\xff\xfe", "<|endoftext|>", "Don't", "\r\n",
}

func (g requestGen) text() string {
	var b strings.Builder
	for range g.intn(7) {
		b.WriteString(textPieces[g.intn(len(textPieces))])
		if g.intn(3) == 0 {
			b.WriteByte(' ')
		}
	}
	if g.intn(25) == 0 {
		b.WriteString(strings.Repeat("abcdefgh ", g.intn(2000)))
	}
	return b.String()
}

func (g requestGen) number() any {
	return []any{0, 1, 7, 100, 4096, -3, 1.5, 1e3, int64(9007199254740991), int64(1) << 62, "12", nil, true, 9007199254740992.0}[g.intn(14)]
}

// wild is any JSON value, nested.
func (g requestGen) wild(depth int) any {
	switch g.intn(8) {
	case 0:
		return g.text()
	case 1:
		return g.number()
	case 2, 3:
		if depth <= 0 {
			return nil
		}
		items := make([]any, g.intn(4))
		for i := range items {
			items[i] = g.wild(depth - 1)
		}
		return items
	case 4, 5:
		if depth <= 0 {
			return nil
		}
		out := map[string]any{}
		for range g.intn(4) {
			out[[]string{"text", "content", "type", "parts", "name", "input", "output", "x"}[g.intn(8)]] = g.wild(depth - 1)
		}
		return out
	}
	return nil
}

func (g requestGen) maybe(v any) any {
	if g.intn(10) == 0 {
		return g.wild(2)
	}
	return v
}

func (g requestGen) part() any {
	switch g.intn(9) {
	case 0:
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + strings.Repeat("A", g.intn(400))}}
	case 1:
		return map[string]any{"type": "input_image", "image_url": g.text()}
	case 2:
		return map[string]any{"type": []string{"input_audio", "input_file", "file"}[g.intn(3)], "x": g.text()}
	case 3:
		return g.text() // a bare string among the parts
	case 4:
		return map[string]any{"type": "refusal", "refusal": g.text()}
	case 5:
		return g.wild(2)
	}
	return map[string]any{"type": []string{"text", "input_text", "output_text"}[g.intn(3)], "text": g.text()}
}

func (g requestGen) content() any {
	switch g.intn(5) {
	case 0:
		return g.text()
	case 1:
		return nil
	}
	parts := make([]any, g.intn(5))
	for i := range parts {
		parts[i] = g.part()
	}
	return parts
}

func (g requestGen) toolCall() any {
	call := map[string]any{"name": g.text(), "arguments": g.maybe(g.text())}
	if g.intn(2) == 0 {
		return map[string]any{"id": "call_" + g.text(), "type": "function", "function": call}
	}
	return call
}

func (g requestGen) message() any {
	if g.intn(12) == 0 {
		return g.wild(2)
	}
	m := map[string]any{}
	if g.intn(8) != 0 {
		m["role"] = []string{"user", "assistant", "system", "tool", "developer", "critic"}[g.intn(6)]
	}
	if g.intn(8) != 0 {
		m["content"] = g.content()
	}
	if g.intn(4) == 0 {
		m["name"] = g.maybe(g.text())
	}
	if g.intn(5) == 0 {
		m["tool_call_id"] = g.maybe(g.text())
	}
	if g.intn(6) == 0 {
		m["call_id"] = g.maybe(g.text())
		m["arguments"] = g.maybe(g.text())
	}
	if g.intn(6) == 0 {
		m["output"] = g.content()
	}
	if g.intn(4) == 0 {
		calls := make([]any, g.intn(3))
		for i := range calls {
			calls[i] = g.toolCall()
		}
		m["tool_calls"] = calls
	}
	return m
}

func (g requestGen) messages() any {
	if g.intn(15) == 0 {
		return g.text()
	}
	out := make([]any, g.intn(6))
	for i := range out {
		out[i] = g.message()
	}
	return out
}

func (g requestGen) schema() any {
	return map[string]any{"type": "object", "properties": map[string]any{g.text(): map[string]any{"type": "string", "description": g.text()}}, "required": []string{g.text()}}
}

func (g requestGen) tools() any {
	if g.intn(10) == 0 {
		return g.wild(3)
	}
	out := make([]any, g.intn(4))
	for i := range out {
		def := map[string]any{"name": g.text(), "description": g.maybe(g.text()), "parameters": g.schema()}
		switch g.intn(3) {
		case 0:
			out[i] = map[string]any{"type": "function", "function": def}
		case 1:
			def["type"] = "function"
			out[i] = def
		default:
			out[i] = map[string]any{"name": g.text(), "input_schema": g.schema()}
		}
	}
	return out
}

// dialect is a native Anthropic, Gemini or Bedrock body fragment.
func (g requestGen) dialect(depth int) any {
	switch g.intn(9) {
	case 0:
		return g.text()
	case 1:
		return map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": strings.Repeat("A", g.intn(300))}}
	case 2:
		return map[string]any{"fileData": map[string]any{"fileUri": g.text()}}
	case 3:
		return map[string]any{"type": "image", "source": map[string]any{"data": strings.Repeat("B", g.intn(300))}}
	}
	if depth <= 0 {
		return g.text()
	}
	switch g.intn(6) {
	case 0:
		items := make([]any, g.intn(4))
		for i := range items {
			items[i] = g.dialect(depth - 1)
		}
		return items
	case 1:
		return map[string]any{"role": []string{"user", "model", "assistant"}[g.intn(3)], "parts": g.dialect(depth - 1)}
	case 2:
		return map[string]any{"role": "user", "content": g.dialect(depth - 1)}
	case 3:
		return map[string]any{"type": "text", "text": g.text()}
	case 4:
		return map[string]any{"functionCall": map[string]any{"name": g.text(), "args": g.wild(1), "input": g.text()}, "functionResponse": g.dialect(depth - 1)}
	}
	return g.wild(2)
}

func (g requestGen) fields(family openai.Family) map[string]json.RawMessage {
	raw := map[string]any{"model": "m"}
	put := func(name string, v any) { raw[name] = v }
	switch g.intn(4) {
	case 0:
		put("messages", g.messages())
	case 1:
		put("input", g.maybe(g.messages()))
	}
	if g.intn(2) == 0 {
		put("messages", g.messages())
	}
	if g.intn(2) == 0 {
		put("input", g.maybe(g.messages()))
	}
	if g.intn(3) == 0 {
		put("instructions", g.maybe(g.text()))
	}
	if g.intn(2) == 0 {
		put("tools", g.tools())
	}
	for _, name := range []string{"system", "contents", "systemInstruction", "generateContentRequest"} {
		if g.intn(3) == 0 {
			put(name, g.dialect(3))
		}
	}
	if g.intn(4) == 0 {
		put("generateContentRequest", map[string]any{"contents": g.dialect(2), "systemInstruction": g.dialect(2)})
	}
	for _, name := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "n"} {
		if g.intn(3) == 0 {
			put(name, g.number())
		}
	}
	if g.intn(4) == 0 {
		put("generationConfig", map[string]any{"maxOutputTokens": g.number(), "candidateCount": g.number()})
	}
	if g.intn(4) == 0 {
		put("inferenceConfig", map[string]any{"maxTokens": g.number()})
	}
	out := make(map[string]json.RawMessage, len(raw))
	for name, v := range raw {
		out[name], _ = json.Marshal(v)
	}
	return out
}

// defaults is what a provider adds to a request: mostly bounds, sometimes the
// instructions the validator allows, and occasionally the input fields it
// forbids, which the walker still reads as the old one did.
func (g requestGen) defaults() map[string]json.RawMessage {
	if g.intn(3) == 0 {
		return nil
	}
	raw := map[string]any{}
	for _, name := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "n"} {
		if g.intn(3) == 0 {
			raw[name] = g.number()
		}
	}
	if g.intn(3) == 0 {
		raw["instructions"] = g.maybe(g.text())
	}
	if g.intn(8) == 0 {
		raw["messages"] = g.messages()
	}
	if g.intn(8) == 0 {
		raw["input"] = g.maybe(g.messages())
	}
	if g.intn(10) == 0 {
		raw["tools"] = g.tools()
	}
	if g.intn(10) == 0 {
		raw["system"] = g.dialect(2)
	}
	if g.intn(6) == 0 {
		raw["generationConfig"] = map[string]any{"maxOutputTokens": g.number(), "candidateCount": g.number()}
	}
	if g.intn(6) == 0 {
		raw["inferenceConfig"] = map[string]any{"maxTokens": g.number()}
	}
	out := make(map[string]json.RawMessage, len(raw))
	for name, v := range raw {
		out[name], _ = json.Marshal(v)
	}
	return out
}

func sameBound(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// walkerReadsMore are the members of a request the walker reads and the old walker
// did not: the schema of a structured output, a model's reasoning and thinking, a
// document block, and Bedrock's tool catalogue. A request that has one is charged
// more than it was, so the requests generated here, which are compared with the
// old walker, must have none; TestWalkerReadsTheStructuredOutputsReasoningAndDocumentsOfEveryDialect
// holds what they cost.
var walkerReadsMore = []string{
	"response_format", `"text":{"format"`, "output_config", "output_format", "outputConfig", "toolConfig",
	"responseSchema", "responseJsonSchema", `"reasoning"`, `"thinking"`, `"redacted_thinking"`, `"document"`, `"summary"`,
}

// TestWalkerMatchesTheLegacyHeuristic holds every family without a tokenizer to
// the estimate the gateway charged before the walker moved, for every shape the
// old walker read: for the same request and provider defaults, the same input,
// reply bound, candidates and total.
func TestWalkerMatchesTheLegacyHeuristic(t *testing.T) {
	cases := 400
	if testing.Short() {
		cases = 60
	}
	counters := []Counter{{}, ForModel("claude-sonnet-4-5"), ForModel("gemini-2.5-pro"), ForModel("mistral-large-latest"), ForModel("my-deployment")}
	// The comparison is only worth something if the requests charge something.
	var charged, flat int
	defer func() {
		if charged < cases*3 || flat < cases/2 {
			t.Errorf("%d estimates charged a prompt and %d charged a flat rate: the generator is not exercising the walker", charged, flat)
		}
	}()
	for _, family := range walkerFamilies {
		t.Run(string(family), func(t *testing.T) {
			g := requestGen{rand.New(rand.NewPCG(7, uint64(len(family))))}
			checked := 0
			for i := range cases {
				fields := g.fields(family)
				parsed := openai.NewEnvelope(family, "route", false, fields)
				prompt := Walk(parsed)
				// One walk answers every provider's defaults.
				for j := range 3 {
					defaults := g.defaults()
					if body, _ := json.Marshal([]any{fields, defaults}); slices.ContainsFunc(walkerReadsMore, func(member string) bool { return strings.Contains(string(body), member) }) {
						t.Fatalf("case %d/%d has a member the old walker did not read, so it cannot be held to it: %s", i, j, body)
					}
					input, output, candidates := legacyEstimateParts(parsed, defaults)
					want := legacyEstimateTokensFromParts(parsed, input, output, candidates)
					for _, c := range counters {
						got := prompt.Estimate(c, defaults)
						if got.Input != input || !sameBound(got.Output, output) || got.Candidates != candidates || got.Tokens() != want {
							body, _ := json.Marshal(fields)
							d, _ := json.Marshal(defaults)
							t.Fatalf("case %d/%d %s on %s\nrequest  %s\ndefaults %s\nlegacy input %d output %v candidates %d total %d\nwalker input %d output %v candidates %d total %d",
								i, j, family, c.Family(), body, d, input, deref(output), candidates, want, got.Input, deref(got.Output), got.Candidates, got.Tokens())
						}
						if got.Provenance != ProvenanceHeuristic || got.Family != c.Family() {
							t.Fatalf("provenance %s family %s, want heuristic %s", got.Provenance, got.Family, c.Family())
						}
					}
					checked++
					if input > 1 {
						charged++
					}
					if input >= ImageTokens {
						flat++
					}
				}
			}
			if checked < cases {
				t.Fatalf("checked %d estimates", checked)
			}
		})
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestWalkerMatchesTheLegacyHeuristicWithoutARequest keeps the estimate of a
// caller that named nothing, whose bounds can only come from provider defaults.
func TestWalkerMatchesTheLegacyHeuristicWithoutARequest(t *testing.T) {
	g := requestGen{rand.New(rand.NewPCG(11, 13))}
	for i := range 300 {
		defaults := g.defaults()
		input, output, candidates := legacyEstimateParts(nil, defaults)
		want := legacyEstimateTokensFromParts(nil, input, output, candidates)
		got := Walk(nil).Estimate(Counter{}, defaults)
		if got.Input != input || !sameBound(got.Output, output) || got.Candidates != candidates || got.Tokens() != want {
			t.Fatalf("case %d: walker %+v, legacy input %d output %v candidates %d total %d", i, got, input, deref(output), candidates, want)
		}
	}
	if got := Walk(nil).Estimate(Counter{}, nil).Tokens(); got != DefaultOutputTokens {
		t.Fatalf("an unnamed request reserves %d, want the default reply %d", got, DefaultOutputTokens)
	}
}

// TestWalkerKeepsTheFlatCharges pins the rates the old walker charged, apart
// from the generated cases that exercise them.
func TestWalkerKeepsTheFlatCharges(t *testing.T) {
	if ImageTokens != legacyImageTokens || MediaTokens != legacyMediaTokens || DefaultOutputTokens != legacyDefaultOutputTokens || MaxTokens != legacyMaxEstimate {
		t.Fatalf("charges moved: %d %d %d %d", ImageTokens, MediaTokens, DefaultOutputTokens, MaxTokens)
	}
	if ImageTokens != 1000 || MediaTokens != 2000 || DefaultOutputTokens != 4096 {
		t.Fatal("the flat charges are part of the contract")
	}
}

func ExampleWalk() {
	request, _ := openai.Parse(openai.FamilyChat, []byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`))
	prompt := Walk(request)
	for _, model := range []string{"gpt-4o", "claude-sonnet-4-5"} {
		e := prompt.Estimate(ForModel(model), nil)
		fmt.Println(model, e.Family, e.Provenance, e.Input, e.Tokens())
	}
	// Output:
	// gpt-4o openai-o200k tokenizer 8 18
	// claude-sonnet-4-5 anthropic heuristic 2 12
}
