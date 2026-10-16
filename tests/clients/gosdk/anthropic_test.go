//go:build integration

package gosdk

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

var anthropicWeather = anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
	Name:        "get_weather",
	Description: anthropic.String("Current weather for a city."),
	InputSchema: anthropic.ToolInputSchemaParam{Properties: weatherParameters["properties"], Required: []string{"city"}},
}}

// text is the concatenated text of a message.
func text(message *anthropic.Message) string {
	var out strings.Builder
	for _, block := range message.Content {
		out.WriteString(block.Text)
	}
	return out.String()
}

func TestAnthropicMessage(t *testing.T) {
	h := connect(t)
	message, err := h.anthropic().Messages.New(timeout(t), anthropic.MessageNewParams{
		Model:     anthropic.Model(h.models["anthropic"]),
		MaxTokens: 64,
		System:    []anthropic.TextBlockParam{{Text: "You are terse."}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Say hello."))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := text(message); got != h.defaultReply || message.StopReason != anthropic.StopReasonEndTurn || string(message.Model) != h.models["anthropic"] {
		t.Fatalf("reply %q (%s) from model %q", got, message.StopReason, message.Model)
	}
	if message.ID == "" || message.Usage.InputTokens == 0 || message.Usage.OutputTokens == 0 {
		t.Fatalf("message %+v", message)
	}

	r := h.onlyRequest(t)
	body := r.json(t)
	if r.Path != "/anthropic/v1/messages" || r.Model != h.upstreamModels["anthropic"] || r.Stream {
		t.Fatalf("upstream saw %+v", r)
	}
	if r.Headers["anthropic-version"] == "" || r.Headers["x-api-key"] != "[present]" {
		t.Fatalf("upstream headers %v", r.Headers)
	}
	expect(t, body, h.upstreamModels["anthropic"], "model")
	expect(t, body, 64.0, "max_tokens")
	expect(t, body, "You are terse.", "system", 0, "text")
	expect(t, body, "Say hello.", "messages", 0, "content", 0, "text")
}

func TestAnthropicMessageStreaming(t *testing.T) {
	h := connect(t)
	stream := h.anthropic().Messages.NewStreaming(timeout(t), anthropic.MessageNewParams{
		Model:     anthropic.Model(h.models["anthropic"]),
		MaxTokens: 64,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Say hello."))},
	})
	defer stream.Close()
	var message anthropic.Message
	deltas := 0
	for stream.Next() {
		event := stream.Current()
		if err := message.Accumulate(event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "content_block_delta" {
			deltas++
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if deltas < 2 {
		t.Fatalf("the text arrived in %d deltas", deltas)
	}
	if got := text(&message); got != h.defaultReply || message.StopReason != anthropic.StopReasonEndTurn || string(message.Model) != h.models["anthropic"] {
		t.Fatalf("assembled %q (%s) from model %q", got, message.StopReason, message.Model)
	}
	if message.Usage.InputTokens == 0 || message.Usage.OutputTokens == 0 {
		t.Fatalf("usage %+v", message.Usage)
	}

	r := h.onlyRequest(t)
	if r.Path != "/anthropic/v1/messages" || !r.Stream || r.Model != h.upstreamModels["anthropic"] {
		t.Fatalf("upstream saw %+v", r)
	}
	expect(t, r.json(t), true, "stream")
}

// toolLoop runs a tool use loop with thinking enabled, as an agent does, and
// returns the final text and the upstream requests. The SDK's own assembly of
// the assistant turn, signed thinking included, is what goes back; the upstream
// rejects a history whose signature changed.
func toolLoop(t *testing.T, h harness, streaming bool, prompt string, results map[string]string) (*anthropic.Message, *anthropic.Message, []record) {
	t.Helper()
	client := h.anthropic()
	messages := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))}
	params := func() anthropic.MessageNewParams {
		return anthropic.MessageNewParams{
			Model: anthropic.Model(h.models["anthropic"]), MaxTokens: 256, Messages: messages,
			Tools: []anthropic.ToolUnionParam{anthropicWeather}, Thinking: anthropic.ThinkingConfigParamOfEnabled(128),
		}
	}

	var assistant anthropic.Message
	if streaming {
		stream := client.Messages.NewStreaming(timeout(t), params())
		defer stream.Close()
		for stream.Next() {
			if err := assistant.Accumulate(stream.Current()); err != nil {
				t.Fatal(err)
			}
		}
		if err := stream.Err(); err != nil {
			t.Fatal(err)
		}
	} else {
		first, err := client.Messages.New(timeout(t), params())
		if err != nil {
			t.Fatal(err)
		}
		assistant = *first
	}
	if assistant.StopReason != anthropic.StopReasonToolUse {
		t.Fatalf("stop reason %q, content %s", assistant.StopReason, mustJSON(assistant.Content))
	}

	messages = append(messages, assistant.ToParam())
	var answers []anthropic.ContentBlockParamUnion
	for _, block := range assistant.Content {
		if block.Type != "tool_use" {
			continue
		}
		use := block.AsToolUse()
		var args struct{ City string }
		if err := json.Unmarshal(use.Input, &args); err != nil || use.Name != "get_weather" || use.ID == "" {
			t.Fatalf("tool use %+v (%v)", use, err)
		}
		answers = append(answers, anthropic.NewToolResultBlock(use.ID, results[args.City], false))
	}
	messages = append(messages, anthropic.NewUserMessage(answers...))

	final, err := client.Messages.New(timeout(t), params())
	if err != nil {
		t.Fatal(err)
	}
	return &assistant, final, h.requests(t, 2)
}

func mustJSON(v any) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func TestAnthropicToolUseLoopWithThinking(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "unary"
		if streaming {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			h := connect(t)
			prompt := "Weather in Paris and Rome? " + tool("get_weather", map[string]any{"city": "Paris"}) + also("get_weather", map[string]any{"city": "Rome"})
			assistant, final, requests := toolLoop(t, h, streaming, prompt, map[string]string{"Paris": "sunny", "Rome": "rainy"})
			if got, want := text(final), h.afterTools("sunny", "rainy"); got != want {
				t.Fatalf("final text %q, want %q", got, want)
			}

			// The turn holds signed thinking and two parallel calls.
			var kinds []string
			var signature string
			var ids []string
			for _, block := range assistant.Content {
				kinds = append(kinds, block.Type)
				switch block.Type {
				case "thinking":
					signature = block.AsThinking().Signature
				case "tool_use":
					ids = append(ids, block.ID)
				}
			}
			if strings.Join(kinds, ",") != "thinking,tool_use,tool_use" || signature == "" || len(ids) != 2 {
				t.Fatalf("assistant content %v, signature %q, tool uses %v", kinds, signature, ids)
			}

			if requests[0].Stream != streaming || requests[1].Stream || requests[0].Script != "tool_call" || requests[1].Script != "tool_result" {
				t.Fatalf("requests %+v", requests)
			}
			first, follow := requests[0].json(t), requests[1].json(t)
			expect(t, first, "enabled", "thinking", "type")
			expect(t, first, "get_weather", "tools", 0, "name")
			expect(t, follow, "assistant", "messages", 1, "role")
			expect(t, follow, "thinking", "messages", 1, "content", 0, "type")
			expect(t, follow, signature, "messages", 1, "content", 0, "signature")
			expect(t, follow, "user", "messages", 2, "role")
			for i, id := range ids {
				expect(t, follow, id, "messages", 1, "content", 1+i, "id")
				expect(t, follow, "tool_result", "messages", 2, "content", i, "type")
				expect(t, follow, id, "messages", 2, "content", i, "tool_use_id")
			}
			expect(t, follow, "sunny", "messages", 2, "content", 0, "content", 0, "text")
			expect(t, follow, "rainy", "messages", 2, "content", 1, "content", 0, "text")
		})
	}
}

func TestAnthropicCountTokens(t *testing.T) {
	h := connect(t)
	client := h.anthropic()
	messages := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("How many tokens is this prompt?"))}
	system := []anthropic.TextBlockParam{{Text: "You are terse."}}
	count, err := client.Messages.CountTokens(timeout(t), anthropic.MessageCountTokensParams{
		Model: anthropic.Model(h.models["anthropic"]), Messages: messages,
		System: anthropic.MessageCountTokensParamsSystemUnion{OfTextBlockArray: system},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The count is what the same prompt reports as its input.
	message, err := client.Messages.New(timeout(t), anthropic.MessageNewParams{
		Model: anthropic.Model(h.models["anthropic"]), MaxTokens: 16, Messages: messages, System: system,
	})
	if err != nil {
		t.Fatal(err)
	}
	if count.InputTokens == 0 || count.InputTokens != message.Usage.InputTokens {
		t.Fatalf("counted %d input tokens, the message reported %d", count.InputTokens, message.Usage.InputTokens)
	}

	requests := h.requests(t, 2)
	if requests[0].Path != "/anthropic/v1/messages/count_tokens" || requests[0].Model != h.upstreamModels["anthropic"] {
		t.Fatalf("upstream saw %+v", requests[0])
	}
	expect(t, requests[0].json(t), "How many tokens is this prompt?", "messages", 0, "content", 0, "text")
	expect(t, requests[0].json(t), "You are terse.", "system", 0, "text")
	if requests[1].Path != "/anthropic/v1/messages" {
		t.Fatalf("upstream saw %+v", requests[1])
	}
}

func TestAnthropicPromptCaching(t *testing.T) {
	h := connect(t)
	client := h.anthropic()
	ask := func(project, question string, ttl anthropic.CacheControlEphemeralTTL) *anthropic.Message {
		t.Helper()
		cache := anthropic.NewCacheControlEphemeralParam()
		cache.TTL = ttl
		shared := strings.Repeat("Shared context of "+project+", long enough to be worth caching. ", 40)
		message, err := client.Messages.New(timeout(t), anthropic.MessageNewParams{
			Model: anthropic.Model(h.models["anthropic"]), MaxTokens: 32,
			System:   []anthropic.TextBlockParam{{Text: shared, CacheControl: cache}},
			Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(question))},
		})
		if err != nil {
			t.Fatal(err)
		}
		return message
	}

	written := ask("project one", "First question.", "")
	if written.Usage.CacheCreationInputTokens == 0 || written.Usage.CacheReadInputTokens != 0 {
		t.Fatalf("the first request should write the cache: %+v", written.Usage)
	}
	read := ask("project one", "A different question.", "")
	if read.Usage.CacheCreationInputTokens != 0 || read.Usage.CacheReadInputTokens != written.Usage.CacheCreationInputTokens {
		t.Fatalf("the second request should read what the first wrote: %+v after %+v", read.Usage, written.Usage)
	}

	// A breakpoint with a one-hour TTL is reported under its own tier.
	hour := ask("project two", "Another question.", anthropic.CacheControlEphemeralTTLTTL1h)
	if hour.Usage.CacheCreation.Ephemeral1hInputTokens == 0 {
		t.Fatalf("the one-hour write is not reported: %+v", hour.Usage)
	}

	requests := h.requests(t, 3)
	expect(t, requests[0].json(t), "ephemeral", "system", 0, "cache_control", "type")
	expect(t, requests[2].json(t), "1h", "system", 0, "cache_control", "ttl")
}

func TestAnthropicBetaFeaturesReachAStrictRoute(t *testing.T) {
	h := connect(t)
	betas := []anthropic.AnthropicBeta{"interleaved-thinking-2025-05-14", "fine-grained-tool-streaming-2025-05-14"}
	message, err := h.anthropic().Beta.Messages.New(timeout(t), anthropic.BetaMessageNewParams{
		Model: anthropic.Model(h.models["anthropic_strict"]), MaxTokens: 64, Betas: betas,
		Messages: []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("Say hello."))},
	})
	if err != nil {
		t.Fatal(err)
	}
	var reply strings.Builder
	for _, block := range message.Content {
		reply.WriteString(block.Text)
	}
	if reply.String() != h.defaultReply {
		t.Fatalf("reply %q", reply.String())
	}
	r := h.onlyRequest(t)
	if r.Path != "/anthropic/v1/messages" || r.Headers["anthropic-beta"] != string(betas[0])+","+string(betas[1]) {
		t.Fatalf("upstream saw %+v", r)
	}
	if r.Model != h.upstreamModels["anthropic"] {
		t.Fatalf("the strict route did not rewrite the model: %+v", r)
	}
}

func TestAnthropicModels(t *testing.T) {
	h := connect(t)
	page, err := h.anthropic().Models.List(timeout(t), anthropic.ModelListParams{Limit: anthropic.Int(100)})
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, model := range page.Data {
		listed[model.ID] = true
	}
	for name, slug := range h.models {
		if !listed[slug] {
			t.Errorf("route %s (%s) is not listed: %v", name, slug, listed)
		}
	}
	h.untouched(t)
}
