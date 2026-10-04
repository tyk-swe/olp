package scripted

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

var weatherUse = map[string]any{
	"name": "get_weather", "description": "weather",
	"input_schema": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
}

func message(h *harness, body map[string]any) response {
	body["model"] = AnthropicModel
	if _, ok := body["max_tokens"]; !ok {
		body["max_tokens"] = 64
	}
	return h.anthropic("/messages", body)
}

func usageOfMessage(t *testing.T, r response) (input, create, read float64) {
	t.Helper()
	want(t, r, 200)
	u := path(r.json(), "usage").(map[string]any)
	return u["input_tokens"].(float64), u["cache_creation_input_tokens"].(float64), u["cache_read_input_tokens"].(float64)
}

// assembled folds a Messages stream into its content blocks.
func assembled(t *testing.T, body []byte) (blocks []map[string]any, stop string, usage map[string]any) {
	t.Helper()
	partial := map[int]string{}
	for _, f := range frames(t, body) {
		var e map[string]any
		if err := json.Unmarshal([]byte(f.data), &e); err != nil || e["type"] != f.event {
			t.Fatalf("frame %q: %v", f.data, err)
		}
		switch f.event {
		case "content_block_start":
			blocks = append(blocks, e["content_block"].(map[string]any))
		case "content_block_delta":
			i, d := int(e["index"].(float64)), e["delta"].(map[string]any)
			b := blocks[i]
			switch d["type"] {
			case "text_delta":
				b["text"] = b["text"].(string) + d["text"].(string)
			case "thinking_delta":
				b["thinking"] = b["thinking"].(string) + d["thinking"].(string)
			case "signature_delta":
				b["signature"] = d["signature"]
			case "input_json_delta":
				partial[i] += d["partial_json"].(string)
			}
		case "content_block_stop":
			i := int(e["index"].(float64))
			if p, ok := partial[i]; ok {
				var input any
				if err := json.Unmarshal([]byte(p), &input); err != nil {
					t.Fatalf("tool input %q: %v", p, err)
				}
				blocks[i]["input"] = input
			}
		case "message_delta":
			stop, usage = path(e, "delta", "stop_reason").(string), e["usage"].(map[string]any)
		}
	}
	return blocks, stop, usage
}

func TestMessagesToolLoopThinkingAndStreamingAgree(t *testing.T) {
	h := newHarness(t)
	body := func(stream bool) map[string]any {
		b := map[string]any{"messages": []any{user(weatherPrompt)}, "tools": []any{weatherUse}, "thinking": map[string]any{"type": "enabled", "budget_tokens": 32}}
		if stream {
			b["stream"] = true
		}
		return b
	}
	unary := message(h, body(false))
	want(t, unary, 200)
	content := unary.json()["content"].([]any)
	if len(content) != 2 || path(content[0], "type") != "thinking" || path(content[0], "signature") != ThinkingSignature ||
		path(content[1], "type") != "tool_use" || path(content[1], "id") != "toolu_fx_1_1" || !reflect.DeepEqual(path(content[1], "input"), map[string]any{"city": "Paris"}) ||
		unary.json()["stop_reason"] != "tool_use" {
		t.Fatalf("unary: %s", unary.body)
	}

	stream := message(h, body(true))
	want(t, stream, 200)
	blocks, stop, usage := assembled(t, stream.body)
	if stop != "tool_use" || len(blocks) != 2 || blocks[0]["thinking"] != path(content[0], "thinking") || blocks[0]["signature"] != ThinkingSignature ||
		blocks[1]["name"] != "get_weather" || !reflect.DeepEqual(blocks[1]["input"], map[string]any{"city": "Paris"}) || usage["output_tokens"].(float64) < 1 {
		t.Fatalf("streamed %v stop %s usage %v", blocks, stop, usage)
	}
	events := []string{}
	for _, f := range frames(t, stream.body) {
		events = append(events, f.event)
	}
	if events[0] != "message_start" || events[1] != "ping" || events[len(events)-1] != "message_stop" {
		t.Fatalf("events %v", events)
	}

	// The follow-up returns the thinking block unchanged, and the tool result.
	followup := []any{user(weatherPrompt),
		map[string]any{"role": "assistant", "content": []any{content[0], content[1]}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_fx_1_1", "content": []any{map[string]any{"type": "text", "text": "sunny"}}}}},
	}
	final := message(h, map[string]any{"messages": followup, "tools": []any{weatherUse}, "thinking": map[string]any{"type": "enabled", "budget_tokens": 32}})
	want(t, final, 200)
	if path(final.json(), "content", 1, "text") != "The tools returned: sunny" || final.json()["stop_reason"] != "end_turn" {
		t.Fatalf("final: %s", final.body)
	}
}

func TestMessagesEnforcesTheToolAndThinkingContracts(t *testing.T) {
	h := newHarness(t)
	use := map[string]any{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": map[string]any{}}
	result := map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "r"}
	assistant := map[string]any{"role": "assistant", "content": []any{use}}
	for name, messages := range map[string][]any{
		"call without a result":   {user("x"), assistant, user("again")},
		"call ends the request":   {user("x"), assistant},
		"result without a call":   {user("x"), map[string]any{"role": "user", "content": []any{result}}},
		"result for another call": {user("x"), assistant, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_2", "content": "r"}}}},
		"altered thinking":        {user("x"), map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "t", "signature": "tampered"}, map[string]any{"type": "text", "text": "a"}}}, user("again")},
		"unsigned thinking":       {user("x"), map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "t"}}}, user("again")},
	} {
		t.Run(name, func(t *testing.T) {
			r := message(h, map[string]any{"messages": messages})
			want(t, r, http.StatusBadRequest)
			if path(r.json(), "type") != "error" || path(r.json(), "error", "type") != "invalid_request_error" {
				t.Fatalf("%s", r.body)
			}
		})
	}
	// A matched pair passes, even with other text next to the result.
	ok := message(h, map[string]any{"messages": []any{user("x"), assistant, map[string]any{"role": "user", "content": []any{result, map[string]any{"type": "text", "text": "<system-reminder>note</system-reminder>"}}}}})
	want(t, ok, 200)
	if path(ok.json(), "content", 0, "text") != "The tools returned: r" {
		t.Fatalf("%s", ok.body)
	}
}

func TestMessagesRequestValidation(t *testing.T) {
	h := newHarness(t)
	ask := []any{user("x")}
	want(t, message(h, map[string]any{"messages": ask, "max_tokens": 0}), http.StatusBadRequest)
	want(t, message(h, map[string]any{"messages": []any{}}), http.StatusBadRequest)
	r := h.do("POST", AnthropicPrefix+"/messages", map[string]string{"X-Api-Key": testCredential}, map[string]any{"model": AnthropicModel, "max_tokens": 4, "messages": ask})
	want(t, r, http.StatusBadRequest) // the version header is required
	r = h.anthropic("/messages", map[string]any{"model": "other", "max_tokens": 4, "messages": ask})
	want(t, r, http.StatusNotFound)
	if path(r.json(), "error", "type") != "not_found_error" {
		t.Fatalf("%s", r.body)
	}
	r = message(h, map[string]any{"messages": []any{user("[[olp:fail 529]]")}})
	want(t, r, 529)
	if path(r.json(), "error", "type") != "overloaded_error" {
		t.Fatalf("%s", r.body)
	}
}

func TestPromptCachingReportsWritesAndReads(t *testing.T) {
	h := newHarness(t)
	beta := map[string]string{"X-Api-Key": testCredential, "Anthropic-Version": "2023-06-01", "Anthropic-Beta": "prompt-caching-2024-07-31"}
	big := strings.Repeat("shared project context. ", 40)
	system := []any{map[string]any{"type": "text", "text": big, "cache_control": map[string]any{"type": "ephemeral"}}}
	ask := func(q string) map[string]any {
		return map[string]any{"model": AnthropicModel, "max_tokens": 16, "system": system, "messages": []any{user(q)}}
	}

	input, create, read := usageOfMessage(t, h.do("POST", AnthropicPrefix+"/messages", beta, ask("first")))
	if create == 0 || read != 0 || input == 0 {
		t.Fatalf("first request: input %v create %v read %v", input, create, read)
	}
	input2, create2, read2 := usageOfMessage(t, h.anthropic("/messages", ask("a different question")))
	if create2 != 0 || read2 != create || input2 == 0 {
		t.Fatalf("second request: input %v create %v read %v, want a read of %v", input2, create2, read2, create)
	}
	// Changing the cached prefix writes again.
	changed := ask("first")
	changed["system"] = []any{map[string]any{"type": "text", "text": big + "edited", "cache_control": map[string]any{"type": "ephemeral"}}}
	_, create3, read3 := usageOfMessage(t, h.anthropic("/messages", changed))
	if create3 == 0 || read3 != 0 {
		t.Fatalf("changed prefix: create %v read %v", create3, read3)
	}
	// A request that marks no block neither writes nor reads.
	plain := ask("x")
	plain["system"] = big
	_, createP, readP := usageOfMessage(t, h.anthropic("/messages", plain))
	if createP != 0 || readP != 0 {
		t.Fatalf("unmarked request: create %v read %v", createP, readP)
	}
	// A second breakpoint reads the first prefix and writes only the difference.
	two := ask("first")
	two["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": strings.Repeat("more context. ", 30), "cache_control": map[string]any{"type": "ephemeral"}}}}}
	_, createT, readT := usageOfMessage(t, h.anthropic("/messages", two))
	if readT != create || createT == 0 {
		t.Fatalf("two breakpoints: create %v read %v, want a read of %v", createT, readT, create)
	}
	// Cache control on tools counts, and so does a one-hour lifetime.
	tools := ask("first")
	tools["tools"] = []any{map[string]any{"name": "t", "input_schema": map[string]any{"type": "object"}, "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}}}
	r := h.anthropic("/messages", tools)
	if _, c, _ := usageOfMessage(t, r); c == 0 || path(r.json(), "usage", "cache_creation", "ephemeral_1h_input_tokens").(float64) != c ||
		path(r.json(), "usage", "cache_creation", "ephemeral_5m_input_tokens").(float64) != 0 {
		t.Fatalf("one-hour write: %s", r.body)
	}
	// Streaming reports the same usage in message_start and message_delta.
	stream := ask("streamed")
	stream["stream"] = true
	_, _, usage := assembled(t, h.anthropic("/messages", stream).body)
	if usage["cache_read_input_tokens"].(float64) != create {
		t.Fatalf("streamed usage %v", usage)
	}
	// The beta header reaches the recording, and nothing was a leak.
	for _, rec := range h.recorded("?dialect=anthropic.messages") {
		if rec.LeakedClientCredential {
			t.Fatalf("leak in %+v", rec)
		}
	}
	if got := h.recorded("")[0].Headers["anthropic-beta"]; got != "prompt-caching-2024-07-31" {
		t.Fatalf("anthropic-beta %q", got)
	}
}

func TestCountTokensAgreesWithMessageUsage(t *testing.T) {
	h := newHarness(t)
	prompt := map[string]any{"model": AnthropicModel, "system": "be brief", "messages": []any{user(strings.Repeat("count these words ", 20))}, "tools": []any{weatherUse}}
	count := h.anthropic("/messages/count_tokens", prompt)
	want(t, count, 200)
	prompt["max_tokens"] = 8
	input, create, read := usageOfMessage(t, h.anthropic("/messages", prompt))
	if got := count.json()["input_tokens"].(float64); got < 1 || got != input+create+read {
		t.Fatalf("count_tokens %v, message usage %v", got, input+create+read)
	}
	want(t, h.anthropic("/messages/count_tokens", map[string]any{"model": "other", "messages": []any{user("x")}}), http.StatusNotFound)
	want(t, h.anthropic("/messages/count_tokens", map[string]any{"model": AnthropicModel}), http.StatusBadRequest)
}

func TestMessagesStructuredOutput(t *testing.T) {
	h := newHarness(t)
	schema := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "integer"}}, "required": []any{"a"}}
	for name, body := range map[string]map[string]any{
		"output_config": {"output_config": map[string]any{"format": map[string]any{"type": "json_schema", "schema": schema}}},
		"output_format": {"output_format": map[string]any{"type": "json_schema", "schema": schema}},
	} {
		t.Run(name, func(t *testing.T) {
			body["messages"] = []any{user("x")}
			r := message(h, body)
			want(t, r, 200)
			if path(r.json(), "content", 0, "text") != `{"a":42}` {
				t.Fatalf("%s", r.body)
			}
		})
	}
	// The pre-structured-output idiom: a forced tool whose input is the object.
	forced := message(h, map[string]any{"messages": []any{user("x")}, "tools": []any{map[string]any{"name": "json", "input_schema": schema}},
		"tool_choice": map[string]any{"type": "tool", "name": "json"}})
	want(t, forced, 200)
	if path(forced.json(), "content", 0, "name") != "json" || !reflect.DeepEqual(path(forced.json(), "content", 0, "input"), map[string]any{"a": float64(42)}) {
		t.Fatalf("%s", forced.body)
	}
	anyTool := message(h, map[string]any{"messages": []any{user("x")}, "tools": []any{weatherUse}, "tool_choice": map[string]any{"type": "any"}})
	if path(anyTool.json(), "content", 0, "input", "city") != "fixture" {
		t.Fatalf("%s", anyTool.body)
	}
}

func TestMessagesRequirePairedBetaHeaders(t *testing.T) {
	h := newHarness(t)
	headers := func(beta string) map[string]string {
		out := map[string]string{"X-Api-Key": testCredential, "Anthropic-Version": "2023-06-01"}
		if beta != "" {
			out["Anthropic-Beta"] = beta
		}
		return out
	}
	system := map[string]any{"role": "system", "content": "reminder"}
	for _, tc := range []struct {
		name, beta, prefix string
		extra              map[string]any
		messages           []any
	}{
		{"context management", "claude-code-20250219", "context-management-", map[string]any{"context_management": map[string]any{"edits": []any{}}}, []any{user("x")}},
		{"safeguards", "context-management-2025-06-27", "dangerous-tool-use-", map[string]any{"safeguards": []any{map[string]any{"type": "dangerous_tool_use"}}}, []any{user("x")}},
		{"effort", "claude-code-20250219", "effort-", map[string]any{"output_config": map[string]any{"effort": "high"}}, []any{user("x")}},
		{"mid-conversation system message", "claude-code-20250219", "mid-conversation-system-", nil, []any{user("x"), system, user("y")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"model": AnthropicModel, "max_tokens": 8, "messages": tc.messages}
			for k, v := range tc.extra {
				body[k] = v
			}
			missing := h.do("POST", AnthropicPrefix+"/messages", headers(tc.beta), body)
			want(t, missing, http.StatusBadRequest)
			if path(missing.json(), "error", "type") != "invalid_request_error" || !strings.Contains(string(missing.body), tc.prefix) {
				t.Fatalf("%s", missing.body)
			}
			// The same request with the paired header passes, whatever else the
			// header lists, and the recording keeps the header.
			paired := h.do("POST", AnthropicPrefix+"/messages", headers(tc.beta+","+tc.prefix+"2026-01-01"), body)
			want(t, paired, 200)
		})
	}
	// A request that uses none of them needs no beta, and structured output in
	// output_config is not an effort.
	want(t, message(h, map[string]any{"messages": []any{user("x")}}), 200)
	want(t, message(h, map[string]any{"messages": []any{user("x")}, "output_config": map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}}}}), 200)
	count := h.do("POST", AnthropicPrefix+"/messages/count_tokens", headers(""), map[string]any{"model": AnthropicModel, "messages": []any{user("x"), system}})
	want(t, count, http.StatusBadRequest) // counting follows the rule of generation
	// Only user, assistant and system are roles.
	want(t, message(h, map[string]any{"messages": []any{user("x"), map[string]any{"role": "tool", "content": "r"}}}), http.StatusBadRequest)
}
