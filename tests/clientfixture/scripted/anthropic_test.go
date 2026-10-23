package scripted

import (
	"net/http"
	"strings"
	"testing"
)

func message(h *harness, body map[string]any) response {
	body["model"] = AnthropicModel
	if _, ok := body["max_tokens"]; !ok {
		body["max_tokens"] = 64
	}
	return h.anthropic("/messages", body)
}

// A matched pair passes, even with other text next to the result.

// the version header is required

// The pre-structured-output idiom: a forced tool whose input is the object.

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
	// output_config is not an effort. A system message between turns needs
	// none either: mid-conversation system messages are generally available.
	want(t, message(h, map[string]any{"messages": []any{user("x")}}), 200)
	want(t, message(h, map[string]any{"messages": []any{user("x")}, "output_config": map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}}}}), 200)
	want(t, message(h, map[string]any{"messages": []any{user("x"), system, user("y")}}), 200)
	count := h.do("POST", AnthropicPrefix+"/messages/count_tokens", headers(""), map[string]any{"model": AnthropicModel, "messages": []any{user("x"), system}})
	want(t, count, 200) // counting follows the rule of generation
	// Only user, assistant and system are roles.
	want(t, message(h, map[string]any{"messages": []any{user("x"), map[string]any{"role": "tool", "content": "r"}}}), http.StatusBadRequest)
}
