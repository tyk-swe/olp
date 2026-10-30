package codewire

import (
	"net/http"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

func ValidateGenericRequest(root oif.Value, headers http.Header, request codemode.Request) error {
	if !GenericConversation(request.Operation.Identity) {
		return nil
	}
	if hasHeader(headers, "X-Codex-Turn-State") || request.PreviousResponse != "" && !strings.HasPrefix(request.Operation.Identity.Conversation, "olp:affinity/") {
		return codemode.Refuse(409, "code_parent_unresolved")
	}
	if stateField(root, "conversation", "container", "context_management") || request.Operation.Name != "responses" && stateField(root, "previous_response_id") {
		return codemode.Refuse(409, "code_parent_unresolved")
	}
	switch request.Operation.Name {
	case "responses":
		if stateContent(field(root, "input")) || stateContent(field(root, "instructions")) {
			return codemode.Refuse(409, "code_parent_unresolved")
		}
	case "messages":
		if stateContent(field(root, "messages")) || stateContent(field(root, "system")) {
			return codemode.Refuse(409, "code_parent_unresolved")
		}
	case string(codemode.ProtocolChat):
		if stateContent(field(root, "messages")) {
			return codemode.Refuse(409, "code_parent_unresolved")
		}
	default:
		return codemode.Refuse(409, "code_parent_unresolved")
	}
	if slices.ContainsFunc(field(root, "tools").Elements(), func(tool oif.Value) bool {
		kind, _ := field(tool, "type").Text()
		return kind != "function" && (kind != "" || field(tool, "name").Kind() != oif.String)
	}) {
		return codemode.Refuse(409, "code_parent_unresolved")
	}
	return nil
}

func stateContent(value oif.Value) bool {
	if value.Kind() == oif.Array {
		return slices.ContainsFunc(value.Elements(), stateContent)
	}
	if value.Kind() != oif.Object {
		return false
	}
	kind, _ := field(value, "type").Text()
	switch kind {
	case "", "message", "tool_result":
		return stateContent(field(value, "content"))
	case "text", "input_text", "output_text", "tool_use", "function_call":
		return false
	case "function_call_output":
		return stateContent(field(value, "output"))
	case "input_image", "input_file":
		return stateField(value, "file_id")
	case "image", "document":
		source := field(value, "source")
		typeName, _ := field(source, "type").Text()
		return typeName != "base64" && typeName != "url" || stateField(source, "file_id")
	default:
		return true
	}
}

func stateField(value oif.Value, names ...string) bool {
	return slices.ContainsFunc(names, func(name string) bool {
		kind := field(value, name).Kind()
		return kind != oif.Absent && kind != oif.Null
	})
}
