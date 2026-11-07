package gateway

import (
	"net/http"
	"slices"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func codeValidateAntigravityResponses(request *wire.Request, r *http.Request) error {
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	if r.Header.Get("X-Codex-Turn-State") != "" || r.Header.Get("OpenAI-Beta") != "" {
		return refused
	}
	root := request.OIF().Document().Root()
	for _, field := range root.Members() {
		if !slices.Contains([]string{"model", "input", "instructions", "stream", "tools", "tool_choice", "max_output_tokens", "temperature", "top_p", "include"}, field.Name) {
			return refused
		}
	}
	input, _ := root.Lookup("input")
	if input.Kind() != oif.String && input.Kind() != oif.Array {
		return refused
	}
	for _, item := range input.Elements() {
		kind, _ := item.Lookup("type")
		switch stringValue(kind) {
		case "", "message":
			role, _ := item.Lookup("role")
			if !slices.Contains([]string{"user", "assistant", "system", "developer"}, stringValue(role)) || !grokTextContent(item, "content") {
				return refused
			}
		case "function_call":
			if !grokString(item, "call_id") || !grokString(item, "name") || !grokString(item, "arguments") {
				return refused
			}
		case "function_call_output":
			if !grokString(item, "call_id") || !grokString(item, "output") {
				return refused
			}
		default:
			return refused
		}
	}
	return nil
}
