package codewire

import (
	"net/http"
	"slices"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

func InitialRecoveryRequest(body []byte, headers http.Header, request codemode.Request) bool {
	if request.Operation.Name != "responses" && request.Operation.Name != "messages" || request.Operation.Identity.Parent != "" || request.PreviousResponse != "" || headers.Get("X-Codex-Turn-State") != "" {
		return false
	}
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: MaxBody})
	if err != nil {
		return false
	}
	for _, name := range []string{"conversation", "previous_response_id", "compaction_trigger"} {
		if value, ok := document.Root().Lookup(name); ok && value.Kind() != oif.Null {
			return false
		}
	}
	if request.Operation.Name == "messages" {
		input, _ := document.Root().Lookup("messages")
		return recoveryInput(input)
	}
	input, _ := document.Root().Lookup("input")
	return recoveryInput(input)
}

func recoveryInput(input oif.Value) bool {
	if input.Kind() == oif.String {
		return true
	}
	if input.Kind() != oif.Array {
		return false
	}
	for _, item := range input.Elements() {
		kind, _ := item.Lookup("type")
		typeName, _ := kind.Text()
		role, _ := item.Lookup("role")
		roleName, _ := role.Text()
		if item.Kind() != oif.Object || typeName != "" && typeName != "message" || roleName != "user" && roleName != "system" && roleName != "developer" {
			return false
		}
		if !recoveryFields(item, "role", "content", "type") {
			return false
		}
		content, _ := item.Lookup("content")
		if content.Kind() == oif.String {
			continue
		}
		if content.Kind() != oif.Array {
			return false
		}
		for _, part := range content.Elements() {
			kind, _ := part.Lookup("type")
			typeName, _ := kind.Text()
			text, _ := part.Lookup("text")
			if typeName != "input_text" && typeName != "text" || text.Kind() != oif.String || !recoveryFields(part, "type", "text") {
				return false
			}
		}
	}
	return true
}

func recoveryFields(value oif.Value, allowed ...string) bool {
	return !slices.ContainsFunc(value.Members(), func(member oif.Member) bool {
		return !slices.Contains(allowed, member.Name)
	})
}
