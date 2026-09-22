package tokenization

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
	"slices"
)

func countDefaults(id string) map[string]operations.Field {
	fields := map[string]operations.Field{}
	switch id {
	case "openai-input-tokens":
		fields["instructions"] = field(map[string]any{"type": []string{"string", "null"}}, nullableString)
		fields["parallel_tool_calls"] = field(map[string]any{"type": []string{"boolean", "null"}}, func(v oif.Value) bool { return v.Kind() == oif.Boolean || v.Kind() == oif.Null })
		for _, name := range []string{"reasoning", "text"} {
			fields[name] = field(map[string]any{"type": []string{"object", "null"}}, nullableObject)
		}
		fields["tools"] = field(map[string]any{"type": []string{"array", "null"}}, nullableArray)
		fields["tool_choice"] = field(map[string]any{"type": []string{"string", "object", "null"}}, func(v oif.Value) bool { return nullableString(v) || v.Kind() == oif.Object })
		fields["truncation"] = field(map[string]any{"enum": []string{"auto", "disabled"}}, func(v oif.Value) bool { return slices.Contains([]string{"auto", "disabled"}, operations.String(v)) })
	case "anthropic-count-tokens":
		fields["system"] = field(map[string]any{"type": []string{"string", "array"}}, func(v oif.Value) bool { return v.Kind() == oif.String || v.Kind() == oif.Array })
		fields["tools"] = field(map[string]any{"type": "array"}, func(v oif.Value) bool { return v.Kind() == oif.Array })
		for _, name := range []string{"thinking", "tool_choice", "output_config"} {
			fields[name] = field(map[string]any{"type": "object"}, func(v oif.Value) bool { return v.Kind() == oif.Object })
		}
		fields["cache_control"] = field(map[string]any{"type": []string{"object", "null"}}, nullableObject)
	}
	return fields
}
func countRequestSchema(id string) []byte {
	switch id {
	case "openai-input-tokens":
		return operations.ObjectSchema(map[string]any{"model": map[string]any{"type": []string{"string", "null"}}, "input": map[string]any{"type": []string{"string", "array", "null"}}})
	case "anthropic-count-tokens":
		return operations.ObjectSchema(map[string]any{"model": map[string]any{"type": "string"}, "messages": map[string]any{"type": "array", "items": map[string]any{"type": "object", "required": []string{"role", "content"}}}}, "model", "messages")
	case "gemini-count-tokens":
		return operations.ObjectSchema(map[string]any{"contents": map[string]any{"type": []string{"array", "null"}}, "generateContentRequest": map[string]any{"type": []string{"object", "null"}}})
	default:
		return operations.ObjectSchema(map[string]any{"input": map[string]any{"type": "object", "minProperties": 1, "maxProperties": 1, "properties": map[string]any{"converse": map[string]any{"type": "object"}, "invokeModel": map[string]any{"type": "object", "required": []string{"body"}}}}}, "input")
	}
}
func countProbe(id, model string) []byte {
	switch id {
	case "openai-input-tokens":
		return operations.Raw(map[string]any{"model": model, "input": "Count this neutral input."})
	case "anthropic-count-tokens":
		return operations.Raw(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "Count this neutral input."}}})
	case "gemini-count-tokens":
		return []byte(`{"contents":[{"role":"user","parts":[{"text":"Count this neutral input."}]}]}`)
	default:
		return []byte(`{"input":{"converse":{"messages":[{"role":"user","content":[{"text":"Count this neutral input."}]}]}}}`)
	}
}
