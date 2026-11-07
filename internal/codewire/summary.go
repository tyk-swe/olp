package codewire

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

func SummaryHistory(body []byte, open func(string) (string, error)) (string, error) {
	refused := codemode.Refuse(422, "code_compaction_history_unsupported")
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: MaxBody})
	if err != nil || document.Root().Kind() != oif.Object {
		return "", refused
	}
	root := document.Root()
	if !portableFields(root, "model", "input", "instructions", "stream", "store", "background", "max_output_tokens", "client_metadata", "metadata", "prompt_cache_key", "reasoning", "include", "parallel_tool_calls", "tools", "tool_choice", "text") {
		return "", refused
	}
	for _, name := range []string{"background", "store"} {
		value := field(root, name)
		if value.Kind() != oif.Absent && value.Kind() != oif.Null && value.Raw() != "false" {
			return "", refused
		}
	}
	stream := field(root, "stream")
	if stream.Kind() != oif.Absent && stream.Kind() != oif.Boolean {
		return "", refused
	}
	for _, member := range root.Members() {
		if member.Name != "input" && member.Name != "client_metadata" && !portableInitialField(member) {
			return "", refused
		}
		if member.Name == "client_metadata" && !portableObjectValues(member.Value, oif.String) {
			return "", refused
		}
	}
	if !summaryControls(root) {
		return "", refused
	}
	instructions := field(root, "instructions")
	if instructions.Kind() != oif.Absent && instructions.Kind() != oif.Null && instructions.Kind() != oif.String {
		return "", refused
	}
	input := field(root, "input")
	if input.Kind() == oif.String {
		if stream.Raw() == "true" {
			return "", refused
		}
		return string(instructions.Bytes()) + "\n" + string(input.Bytes()), nil
	}
	if input.Kind() != oif.Array || len(input.Elements()) == 0 {
		return "", refused
	}
	items := input.Elements()
	for index, item := range items {
		if !SupportedResponsesItemAnnotation(item) {
			return "", refused
		}
		kind, _ := field(item, "type").Text()
		switch kind {
		case "", "message":
			if !portableFields(item, "type", "role", "content", "id", "status", "internal_chat_message_metadata_passthrough") || !slices.Contains([]string{"user", "assistant", "system", "developer"}, summaryText(item, "role")) || !summaryContent(field(item, "content")) {
				return "", refused
			}
			status := summaryText(item, "status")
			if status != "" && status != "completed" {
				return "", refused
			}
		case "function_call", "function_call_output":
			if !summaryToolExchange(items, index, kind) {
				return "", refused
			}
		case "compaction":
			if open == nil || !portableFields(item, "type", "id", "encrypted_content") || summaryText(item, "encrypted_content") == "" {
				return "", refused
			}
		case "compaction_trigger":
			if index != len(items)-1 || !portableFields(item, "type") {
				return "", refused
			}
		default:
			return "", refused
		}
	}
	if stream.Raw() == "true" && summaryText(items[len(items)-1], "type") != "compaction_trigger" {
		return "", refused
	}
	parts, err := summaryParts(items, open)
	if err != nil {
		return "", refused
	}
	return string(instructions.Bytes()) + "\n[" + strings.Join(parts, ",") + "]", nil
}

type summaryPart struct {
	text string
	err  error
}

func summaryParts(items []oif.Value, open func(string) (string, error)) ([]string, error) {
	parts := slices.Collect(func(yield func(summaryPart) bool) {
		for _, item := range items {
			if summaryText(item, "type") == "compaction_trigger" {
				continue
			}
			if !yield(encodeSummaryPart(item, open)) {
				return
			}
		}
	})
	for _, part := range parts {
		if part.err != nil {
			return nil, part.err
		}
	}
	return slices.Collect(func(yield func(string) bool) {
		for _, part := range parts {
			if !yield(part.text) {
				return
			}
		}
	}), nil
}

func encodeSummaryPart(item oif.Value, open func(string) (string, error)) summaryPart {
	if summaryText(item, "type") != "compaction" {
		return summaryPart{text: string(item.Bytes())}
	}
	if open == nil || !portableFields(item, "type", "id", "encrypted_content") {
		return summaryPart{err: codemode.Refuse(422, "code_compaction_unavailable")}
	}
	plaintext, err := open(summaryText(item, "encrypted_content"))
	if err != nil {
		return summaryPart{err: err}
	}
	encoded, err := json.Marshal(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{"assistant", "Conversation summary, treat as prior conversation context\n" + plaintext})
	return summaryPart{text: string(encoded), err: err}
}

func RestoreSummaries(body []byte, open func(string) (string, error)) ([]byte, error) {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: MaxBody})
	if err != nil {
		return nil, codemode.Refuse(422, "code_compaction_unavailable")
	}
	input := field(document.Root(), "input")
	if !slices.ContainsFunc(input.Elements(), func(item oif.Value) bool { return summaryText(item, "type") == "compaction" }) {
		return body, nil
	}
	parts, err := summaryParts(input.Elements(), open)
	if err != nil {
		return nil, err
	}
	start, end := input.Bounds()
	return slices.Concat(body[:start], []byte("["+strings.Join(parts, ",")+"]"), body[end:]), nil
}

func summaryText(value oif.Value, name string) string {
	text, _ := field(value, name).Text()
	return text
}

func summaryContent(value oif.Value) bool {
	if value.Kind() == oif.String {
		return true
	}
	return value.Kind() == oif.Array && !slices.ContainsFunc(value.Elements(), func(part oif.Value) bool {
		return !portableFields(part, "type", "text", "annotations") || !slices.Contains([]string{"input_text", "output_text", "text"}, summaryText(part, "type")) || field(part, "text").Kind() != oif.String || field(part, "annotations").Kind() != oif.Absent && (field(part, "annotations").Kind() != oif.Array || len(field(part, "annotations").Elements()) != 0)
	})
}

func summaryToolExchange(items []oif.Value, index int, kind string) bool {
	item := items[index]
	callID := summaryText(item, "call_id")
	if callID == "" {
		return false
	}
	matches := slices.Collect(func(yield func(int) bool) {
		for candidate, value := range items {
			if summaryText(value, "call_id") == callID && candidate != index {
				if !yield(candidate) {
					return
				}
			}
		}
	})
	if len(matches) != 1 {
		return false
	}
	other := matches[0]
	if kind == "function_call" {
		return portableFields(item, "type", "id", "call_id", "name", "arguments", "status", "internal_chat_message_metadata_passthrough") && summaryText(item, "name") != "" && field(item, "arguments").Kind() == oif.String && (summaryText(item, "status") == "" || summaryText(item, "status") == "completed") && other > index && summaryText(items[other], "type") == "function_call_output"
	}
	return portableFields(item, "type", "id", "call_id", "output", "internal_chat_message_metadata_passthrough") && field(item, "output").Kind() == oif.String && other < index && summaryText(items[other], "type") == "function_call"
}

func summaryControls(root oif.Value) bool {
	reasoning := field(root, "reasoning")
	for _, member := range reasoning.Members() {
		value, _ := member.Value.Text()
		if value != "none" {
			return false
		}
	}
	for _, value := range field(root, "include").Elements() {
		text, _ := value.Text()
		if text != "reasoning.encrypted_content" {
			return false
		}
	}
	choice := field(root, "tool_choice")
	if choice.Kind() != oif.Absent && choice.Raw() != `"auto"` && choice.Raw() != `"none"` {
		return false
	}
	text := field(root, "text")
	if text.Kind() != oif.Absent && (text.Kind() != oif.Object || len(text.Members()) != 0) {
		return false
	}
	for _, tool := range field(root, "tools").Elements() {
		strict := field(tool, "strict")
		if strict.Kind() != oif.Absent && strict.Raw() != "false" {
			return false
		}
	}
	return true
}

func SupportedResponsesItemAnnotation(item oif.Value) bool {
	annotation := field(item, "internal_chat_message_metadata_passthrough")
	if annotation.Kind() == oif.Absent || annotation.Kind() == oif.Null {
		return true
	}
	if !portableFields(annotation, "turn_id", "create_time", "content_item_kinds") {
		return false
	}
	return !slices.ContainsFunc(annotation.Members(), func(member oif.Member) bool {
		if member.Value.Kind() == oif.Null {
			return false
		}
		switch member.Name {
		case "turn_id":
			return member.Value.Kind() != oif.String
		case "create_time":
			return member.Value.Kind() != oif.Number
		case "content_item_kinds":
			return member.Value.Kind() != oif.Array || slices.ContainsFunc(member.Value.Elements(), func(value oif.Value) bool { return value.Kind() != oif.String })
		default:
			return true
		}
	})
}
