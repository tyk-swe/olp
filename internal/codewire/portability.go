package codewire

import (
	"net/http"
	"slices"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

func PortableInitialRequest(body []byte, headers http.Header, request codemode.Request) bool {
	if request.Operation.Name != "responses" && request.Operation.Name != "messages" && request.Operation.Name != "chat" || request.Operation.Identity.Parent != "" || request.PreviousResponse != "" || len(headers.Values("X-Codex-Turn-State")) != 0 {
		return false
	}
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: MaxBody})
	if err != nil {
		return false
	}
	root := document.Root()
	if request.Operation.Name == "chat" {
		return portableInitialChat(root)
	}
	if request.Operation.Name == "messages" {
		if !portableFields(root, "model", "messages", "system", "max_tokens", "stream", "temperature", "top_p", "top_k", "stop_sequences", "metadata", "tools", "tool_choice") {
			return false
		}
	} else if !portableFields(root, "model", "input", "instructions", "stream", "store", "background", "max_output_tokens", "temperature", "top_p", "parallel_tool_calls", "service_tier", "truncation", "max_tool_calls", "safety_identifier", "user", "prompt_cache_key", "prompt_cache_retention", "metadata", "reasoning", "text", "include", "stream_options", "tools", "tool_choice", "conversation", "previous_response_id", "compaction_trigger") {
		return false
	}
	return !slices.ContainsFunc(root.Members(), func(member oif.Member) bool {
		if request.Operation.Name == "messages" {
			return !portableMessagesField(member)
		}
		return !portableInitialField(member)
	})
}

func portableMessagesField(member oif.Member) bool {
	if member.Value.Kind() == oif.Null {
		return portableInitialField(member)
	}
	switch member.Name {
	case "tools":
		return member.Value.Kind() == oif.Array && !slices.ContainsFunc(member.Value.Elements(), func(tool oif.Value) bool {
			nameValue, _ := tool.Lookup("name")
			name, _ := nameValue.Text()
			schema, _ := tool.Lookup("input_schema")
			return name == "" || schema.Kind() != oif.Object || !portableFields(tool, "name", "description", "input_schema", "eager_input_streaming") || slices.ContainsFunc(tool.Members(), func(field oif.Member) bool {
				switch field.Name {
				case "input_schema":
					return field.Value.Kind() != oif.Object
				case "eager_input_streaming":
					return field.Value.Kind() != oif.Boolean
				default:
					return field.Value.Kind() != oif.String
				}
			})
		})
	case "tool_choice":
		typeValue, _ := member.Value.Lookup("type")
		kind, _ := typeValue.Text()
		nameValue, hasName := member.Value.Lookup("name")
		name, _ := nameValue.Text()
		return slices.Contains([]string{"auto", "any", "none", "tool"}, kind) && (kind != "tool" || name != "") && (!hasName || kind == "tool") && portableFields(member.Value, "type", "name", "disable_parallel_tool_use") && !slices.ContainsFunc(member.Value.Members(), func(field oif.Member) bool {
			if field.Name == "disable_parallel_tool_use" {
				return field.Value.Kind() != oif.Boolean
			}
			return field.Value.Kind() != oif.String
		})
	default:
		return portableInitialField(member)
	}
}

func portableInitialField(member oif.Member) bool {
	value := member.Value
	if value.Kind() == oif.Null && member.Name != "input" && member.Name != "messages" {
		return true
	}
	switch member.Name {
	case "input", "messages":
		return portableInitialInput(value)
	case "system":
		return portableTextContent(value)
	case "model", "instructions", "service_tier", "truncation", "safety_identifier", "user", "prompt_cache_key", "prompt_cache_retention":
		return value.Kind() == oif.String
	case "stream", "store", "background", "parallel_tool_calls":
		return value.Kind() == oif.Boolean
	case "max_output_tokens", "max_tokens", "max_tool_calls", "temperature", "top_p", "top_k":
		return value.Kind() == oif.Number
	case "metadata":
		return portableObjectValues(value, oif.String)
	case "reasoning":
		return portableFields(value, "effort", "summary") && portableObjectValues(value, oif.String)
	case "text":
		return portableFields(value, "verbosity") && portableObjectValues(value, oif.String)
	case "stream_options":
		return portableFields(value, "include_usage", "include_obfuscation") && portableObjectValues(value, oif.Boolean)
	case "include", "stop_sequences":
		return value.Kind() == oif.Array && !slices.ContainsFunc(value.Elements(), func(element oif.Value) bool { return element.Kind() != oif.String })
	case "tools":
		return value.Kind() == oif.Array && !slices.ContainsFunc(value.Elements(), func(tool oif.Value) bool {
			typeValue, _ := tool.Lookup("type")
			name, _ := typeValue.Text()
			return name != "function" || !portableFunction(tool)
		})
	case "tool_choice":
		name, _ := value.Text()
		if value.Kind() == oif.String {
			return slices.Contains([]string{"auto", "none", "required"}, name)
		}
		typeValue, _ := value.Lookup("type")
		typeName, _ := typeValue.Text()
		return typeName == "function" && portableFields(value, "type", "name") && portableObjectValues(value, oif.String)
	default:
		return false
	}
}

func portableObjectValues(value oif.Value, kind oif.Kind) bool {
	return value.Kind() == oif.Object && !slices.ContainsFunc(value.Members(), func(member oif.Member) bool { return member.Value.Kind() != kind })
}

func portableInitialInput(input oif.Value) bool {
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
		if !portableFields(item, "role", "content", "type") {
			return false
		}
		content, _ := item.Lookup("content")
		if !portableTextContent(content) {
			return false
		}
	}
	return true
}

func portableTextContent(content oif.Value) bool {
	if content.Kind() == oif.String {
		return true
	}
	if content.Kind() != oif.Array {
		return false
	}
	return !slices.ContainsFunc(content.Elements(), func(part oif.Value) bool {
		kind, _ := part.Lookup("type")
		typeName, _ := kind.Text()
		text, _ := part.Lookup("text")
		return typeName != "input_text" && typeName != "text" || text.Kind() != oif.String || !portableFields(part, "type", "text")
	})
}

func portableFields(value oif.Value, allowed ...string) bool {
	return value.Kind() == oif.Object && !slices.ContainsFunc(value.Members(), func(member oif.Member) bool {
		return !slices.Contains(allowed, member.Name)
	})
}

func portableInitialChat(root oif.Value) bool {
	if !portableFields(root, "model", "messages", "stream", "stream_options", "max_tokens", "max_completion_tokens", "temperature", "top_p", "parallel_tool_calls", "tools", "tool_choice", "reasoning_effort") {
		return false
	}
	return !slices.ContainsFunc(root.Members(), func(member oif.Member) bool {
		switch member.Name {
		case "reasoning_effort":
			value, _ := member.Value.Text()
			return !slices.Contains([]string{"low", "medium", "high"}, value)
		case "max_completion_tokens":
			return member.Value.Kind() != oif.Number
		case "tools":
			return member.Value.Kind() != oif.Array || slices.ContainsFunc(member.Value.Elements(), func(tool oif.Value) bool {
				typeValue, _ := tool.Lookup("type")
				kind, _ := typeValue.Text()
				function, _ := tool.Lookup("function")
				return kind != "function" || !portableFields(tool, "type", "function") || !portableFunction(function)
			})
		case "tool_choice":
			if member.Value.Kind() == oif.String || member.Value.Kind() == oif.Null {
				return !portableInitialField(member)
			}
			typeValue, _ := member.Value.Lookup("type")
			kind, _ := typeValue.Text()
			function, _ := member.Value.Lookup("function")
			return kind != "function" || !portableFields(member.Value, "type", "function") || !portableFields(function, "name") || !portableObjectValues(function, oif.String)
		case "stream_options":
			return !portableFields(member.Value, "include_usage") || !portableObjectValues(member.Value, oif.Boolean)
		default:
			return !portableInitialField(member)
		}
	})
}

func portableFunction(function oif.Value) bool {
	return portableFields(function, "type", "name", "description", "parameters", "strict") && !slices.ContainsFunc(function.Members(), func(field oif.Member) bool {
		switch field.Name {
		case "parameters":
			return field.Value.Kind() != oif.Object
		case "strict":
			return field.Value.Kind() != oif.Boolean && field.Value.Kind() != oif.Null
		default:
			return field.Value.Kind() != oif.String
		}
	})
}
