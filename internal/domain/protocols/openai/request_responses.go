package openai

import (
	"encoding/json"
	"fmt"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

func validateResponses(fields map[string]json.RawMessage, doc oif.Value) error {
	if doc.Kind() == oif.Object {
		input, _ := doc.Lookup("input")
		if err := validateResponsesInputValue(input); err != nil {
			return err
		}
	} else if err := ValidateResponsesInput(fields["input"]); err != nil {
		return err
	}
	if raw, present := fields["previous_response_id"]; present && !isNull(raw) {
		if _, ok := stringField(fields, "previous_response_id"); !ok {
			return invalid("previous_response_id", "previous_response_id must be a string.")
		}
	}
	if raw, present := fields["conversation"]; present && !isNull(raw) {
		var id string
		if json.Unmarshal(raw, &id) != nil {
			var conversation map[string]json.RawMessage
			if json.Unmarshal(raw, &conversation) != nil || json.Unmarshal(conversation["id"], &id) != nil {
				return invalid("conversation", "conversation must be an identifier or an object with an identifier.")
			}
		}
		if id == "" {
			return invalid("conversation", "conversation must have a nonempty identifier.")
		}
	}
	for _, name := range []string{"background", "store"} {
		if raw, present := fields[name]; present && !isNull(raw) {
			var flag bool
			if json.Unmarshal(raw, &flag) != nil {
				return invalid(name, name+" must be a boolean.")
			}
		}
	}
	if raw, present := fields["instructions"]; present && !isNull(raw) {
		if _, ok := stringField(fields, "instructions"); !ok {
			return invalid("instructions", "instructions must be a string.")
		}
	}
	for _, name := range []string{"max_output_tokens", "max_tool_calls"} {
		if err := integerField(fields, name, true); err != nil {
			return err
		}
	}
	for _, name := range []string{"temperature", "top_p"} {
		if err := numberField(fields, name); err != nil {
			return err
		}
	}
	return toolsField(fields)
}

// ValidateResponsesInput validates inline Responses input and rejects
// account-scoped references, both on ingress and after hosting rewrites.
func ValidateResponsesInput(raw json.RawMessage) error {
	if len(raw) == 0 || isNull(raw) {
		return &wire.RequestError{Code: "missing_required_parameter", Message: "input is required.", Param: "input"}
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return invalid("input", "input must be a string or a non-empty array.")
	}
	for i, item := range items {
		if err := validateResponseInput(item, fmt.Sprintf("input[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

// validateResponseInput rejects account-scoped references while retaining
// inline messages and tool results for stateless replay across credentials.
func validateResponseInput(raw json.RawMessage, path string) error {
	item, err := object(raw)
	if err != nil {
		return invalid(path, "Each input item must be an object.")
	}
	kind, _ := stringField(item, "type")
	_, hasID := item["id"]
	if wire.IsResponsesItemReference(kind, hasID) {
		return &wire.RequestError{Code: "unsupported_stateful_reference", Message: "Input item references are not supported; send the full item inline.", Param: path}
	}
	if err := validateResponseFileReference(item, path); err != nil {
		return err
	}
	var field string
	switch kind {
	case "", "message":
		field = "content"
	case "function_call_output", "custom_tool_call_output", "computer_call_output":
		field = "output"
	default:
		return nil
	}
	parts, array := arrayField(item, field)
	if !array {
		// Computer screenshots are a single object; text inputs and tool
		// results remain opaque strings and are never interpreted as JSON.
		parts = []json.RawMessage{item[field]}
	}
	for i, raw := range parts {
		part, err := object(raw)
		if err != nil {
			continue
		}
		param := path + "." + field
		if array {
			param += fmt.Sprintf("[%d]", i)
		}
		if err := validateResponseFileReference(part, param); err != nil {
			return err
		}
	}
	return nil
}

// validateResponsesInputValue is ValidateResponsesInput for a value of a parsed
// document, which is read where it lies.
func validateResponsesInputValue(input oif.Value) error {
	switch input.Kind() {
	case oif.Absent, oif.Null:
		return &wire.RequestError{Code: "missing_required_parameter", Message: "input is required.", Param: "input"}
	case oif.String:
		return nil
	}
	items := input.Elements()
	if len(items) == 0 {
		return invalid("input", "input must be a string or a non-empty array.")
	}
	for i, item := range items {
		if err := validateResponseInputValue(item, fmt.Sprintf("input[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

// validateResponseInputValue is validateResponseInput for a value of a parsed
// document.
func validateResponseInputValue(item oif.Value, path string) error {
	if item.Kind() != oif.Object {
		return invalid(path, "Each input item must be an object.")
	}
	kind := memberText(item, "type")
	_, hasID := item.Lookup("id")
	if wire.IsResponsesItemReference(kind, hasID) {
		return &wire.RequestError{Code: "unsupported_stateful_reference", Message: "Input item references are not supported; send the full item inline.", Param: path}
	}
	if err := validateResponseFileReferenceValue(item, path); err != nil {
		return err
	}
	var field string
	switch kind {
	case "", "message":
		field = "content"
	case "function_call_output", "custom_tool_call_output", "computer_call_output":
		field = "output"
	default:
		return nil
	}
	content, _ := item.Lookup(field)
	parts, array := content.Elements(), content.Kind() == oif.Array
	if !array {
		// Computer screenshots are a single object; text inputs and tool
		// results remain opaque strings and are never interpreted as JSON.
		parts = []oif.Value{content}
	}
	for i, part := range parts {
		if part.Kind() != oif.Object {
			continue
		}
		param := path + "." + field
		if array {
			param += fmt.Sprintf("[%d]", i)
		}
		if err := validateResponseFileReferenceValue(part, param); err != nil {
			return err
		}
	}
	return nil
}

// memberText is a member of an object that is a string, and nothing for any
// other value.
func memberText(object oif.Value, name string) string {
	member, _ := object.Lookup(name)
	text, _ := member.Chars()
	return text
}

func validateResponseFileReferenceValue(part oif.Value, path string) error {
	switch memberText(part, "type") {
	case "input_file", "input_image", "computer_screenshot":
		if fileID, present := part.Lookup("file_id"); present && fileID.Kind() != oif.Null {
			if id, ok := fileID.Text(); !ok || id == "" {
				return invalid(path+".file_id", "file_id must be a nonempty identifier.")
			}
		}
	}
	return nil
}

func validateResponseFileReference(part map[string]json.RawMessage, path string) error {
	kind, _ := stringField(part, "type")
	switch kind {
	case "input_file", "input_image", "computer_screenshot":
		if raw, present := part["file_id"]; present && !isNull(raw) {
			var id string
			if json.Unmarshal(raw, &id) != nil || id == "" {
				return invalid(path+".file_id", "file_id must be a nonempty identifier.")
			}
		}
	}
	return nil
}
