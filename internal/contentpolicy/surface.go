package contentpolicy

import (
	"slices"

	"github.com/tyk-swe/olp/internal/oif"
)

// InputTexts preserves the legacy native input matching contract, including
// individual schema keys, scalar values, and decoded tool arguments.
func InputTexts(document oif.Document) []string {
	texts := []string{}
	for _, name := range []string{"messages", "input", "prompt", "contents", "system", "systemInstruction", "instructions", "tools", "toolConfig", "tool_choice", "response_format", "text", "generationConfig"} {
		if value, present := document.Root().Lookup(name); present {
			collectPolicyText(value, &texts, name == "tools" || name == "toolConfig" || name == "response_format" || name == "text" || name == "generationConfig")
		}
	}
	return texts
}

func collectPolicyText(value oif.Value, texts *[]string, schema bool) {
	switch value.Kind() {
	case oif.String:
		text, _ := value.Text()
		*texts = append(*texts, text)
	case oif.Number, oif.Boolean:
		if schema {
			*texts = append(*texts, value.Raw())
		}
	case oif.Array:
		for _, element := range value.Elements() {
			collectPolicyText(element, texts, schema)
		}
	case oif.Object:
		for _, field := range value.Members() {
			if !schema && slices.Contains([]string{"role", "type", "id", "call_id", "status", "tool_call_id", "tool_use_id"}, field.Name) {
				continue
			}
			if schema {
				*texts = append(*texts, field.Name)
			}
			if field.Name == "arguments" && field.Value.Kind() == oif.String {
				text, _ := field.Value.Text()
				if arguments, err := oif.ParseJSON([]byte(text), oif.Limits{MaxBytes: 8 << 20}); err == nil {
					collectPolicyText(arguments.Root(), texts, true)
				}
			}
			// Structured tool arguments/results are inspected like parsed OpenAI
			// arguments, so text in key or scalar positions cannot bypass rules.
			structured := field.Value.Kind() == oif.Object && slices.Contains([]string{"input", "args", "response"}, field.Name)
			collectPolicyText(field.Value, texts, schema || structured || field.Name == "parameters" || field.Name == "input_schema")
		}
	}
}
