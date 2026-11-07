package gateway

import (
	"net/http"
	"slices"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func codeTranslateGrok(body []byte, r *http.Request, model string) (codePayload, error) {
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	if r.URL.RawQuery != "" || len(r.Trailer) != 0 || r.Header.Get("X-Codex-Turn-State") != "" || r.Header.Get("OpenAI-Beta") != "" {
		return codePayload{}, refused
	}
	request, err := protocols.ParseForModel(wire.Responses, body, model)
	if err != nil {
		return codePayload{}, refused
	}
	root := request.OIF().Document().Root()
	for _, field := range root.Members() {
		if !slices.Contains([]string{"model", "input", "instructions", "stream", "store", "tools", "tool_choice", "parallel_tool_calls", "max_output_tokens", "temperature", "top_p", "text", "reasoning", "metadata", "prompt_cache_key", "safety_identifier", "include"}, field.Name) {
			return codePayload{}, refused
		}
	}
	stored, _ := root.Lookup("store")
	if stored.Kind() != oif.Absent && stored.Kind() != oif.Null && string(stored.Raw()) != "false" {
		return codePayload{}, refused
	}
	include, _ := root.Lookup("include")
	if include.Kind() != oif.Absent && include.Kind() != oif.Null && (include.Kind() != oif.Array || len(include.Elements()) != 0) {
		return codePayload{}, refused
	}
	input, _ := root.Lookup("input")
	if input.Kind() != oif.String && input.Kind() != oif.Array {
		return codePayload{}, refused
	}
	for _, item := range input.Elements() {
		kind, _ := item.Lookup("type")
		switch stringValue(kind) {
		case "", "message":
			role, _ := item.Lookup("role")
			if !slices.Contains([]string{"user", "assistant", "system", "developer"}, stringValue(role)) || !grokTextContent(item, "content") {
				return codePayload{}, refused
			}
		case "function_call":
			if !grokString(item, "call_id") || !grokString(item, "name") || !grokString(item, "arguments") {
				return codePayload{}, refused
			}
		case "function_call_output":
			if !grokString(item, "call_id") || !grokString(item, "output") {
				return codePayload{}, refused
			}
		default:
			return codePayload{}, refused
		}
	}
	tools, _ := root.Lookup("tools")
	for _, tool := range tools.Elements() {
		kind, _ := tool.Lookup("type")
		if stringValue(kind) != "function" {
			return codePayload{}, refused
		}
	}
	return codePayload{body: body, stream: request.Stream, target: codeadapter.TargetGrok}, nil
}

func grokString(item oif.Value, field string) bool {
	value, _ := item.Lookup(field)
	return value.Kind() == oif.String
}

func grokTextContent(item oif.Value, field string) bool {
	content, _ := item.Lookup(field)
	if content.Kind() == oif.String {
		return true
	}
	if content.Kind() != oif.Array {
		return false
	}
	for _, part := range content.Elements() {
		kind, _ := part.Lookup("type")
		if !slices.Contains([]string{"input_text", "output_text"}, stringValue(kind)) || !grokString(part, "text") {
			return false
		}
	}
	return true
}
