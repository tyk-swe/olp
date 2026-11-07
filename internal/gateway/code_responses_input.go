package gateway

import (
	"slices"

	"github.com/tyk-swe/olp/internal/oif"
)

func codeResponsesInput(input oif.Value) bool {
	if input.Kind() != oif.String && input.Kind() != oif.Array {
		return false
	}
	for _, item := range input.Elements() {
		kind, _ := item.Lookup("type")
		switch stringValue(kind) {
		case "", "message":
			role, _ := item.Lookup("role")
			if !slices.Contains([]string{"user", "assistant", "system", "developer"}, stringValue(role)) || !codeResponsesTextContent(item, "content") {
				return false
			}
		case "function_call":
			if !codeResponsesString(item, "call_id") || !codeResponsesString(item, "name") || !codeResponsesString(item, "arguments") {
				return false
			}
		case "function_call_output":
			if !codeResponsesString(item, "call_id") || !codeResponsesString(item, "output") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func codeResponsesString(item oif.Value, field string) bool {
	value, _ := item.Lookup(field)
	return value.Kind() == oif.String
}

func codeResponsesTextContent(item oif.Value, field string) bool {
	content, _ := item.Lookup(field)
	if content.Kind() == oif.String {
		return true
	}
	if content.Kind() != oif.Array {
		return false
	}
	for _, part := range content.Elements() {
		kind, _ := part.Lookup("type")
		if !slices.Contains([]string{"input_text", "output_text"}, stringValue(kind)) || !codeResponsesString(part, "text") {
			return false
		}
	}
	return true
}
