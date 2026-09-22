package tokenization

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
	"slices"
	"strconv"
	"strings"
)

func policyConflict(path string) error {
	return operations.Error("policy_conflict", path, "native_text_coverage", "The policy cannot inspect this native input scope.")
}
func known(root oif.Value, names ...string) error {
	for _, member := range root.Members() {
		if !slices.Contains(names, member.Name) {
			return policyConflict(operations.Pointer("", member.Name))
		}
	}
	return nil
}
func collect(value oif.Value, path string, schema bool) ([]operations.Text, error) {
	var out []operations.Text
	switch value.Kind() {
	case oif.String:
		out = append(out, operations.Text{Pointer: path, Value: operations.String(value)})
	case oif.Array:
		for i, item := range value.Elements() {
			texts, err := collect(item, path+"/"+strconv.Itoa(i), schema)
			if err != nil {
				return nil, err
			}
			out = append(out, texts...)
		}
	case oif.Object:
		for _, member := range value.Members() {
			p := operations.Pointer(path, member.Name)
			childSchema := schema || slices.Contains([]string{"schema", "input_schema", "parameters", "args", "json"}, member.Name) ||
				(member.Name == "input" && (operations.String(operations.Member(value, "type")) == "tool_use" || strings.HasSuffix(path, "/toolUse"))) ||
				(member.Name == "response" && strings.HasSuffix(path, "/functionResponse"))
			if member.Name == "arguments" && operations.String(operations.Member(value, "type")) == "function_call" {
				parsed, err := oif.ParseJSON([]byte(operations.String(member.Value)), oif.Limits{})
				if err != nil {
					return nil, policyConflict(p)
				}
				decoded, err := collect(parsed.Root(), p, true)
				if err != nil {
					return nil, err
				}
				out = append(out, decoded...)
			}
			if !schema && slices.Contains([]string{"data", "bytes", "body", "image_url", "imageUrl", "url", "file_data", "fileData", "inline_data", "inlineData", "encrypted_content", "encryptedContent", "file_id", "cachedContent"}, member.Name) && !operations.Optional(member.Value) {
				return nil, policyConflict(p)
			}
			if schema {
				out = append(out, operations.Text{Pointer: p, Value: member.Name})
			}
			texts, err := collect(member.Value, p, childSchema)
			if err != nil {
				return nil, err
			}
			out = append(out, texts...)
		}
	}
	return out, nil
}
func countText(request oif.Request, id string) ([]operations.Text, error) {
	view, err := countRequest(request, id)
	if err != nil {
		return nil, err
	}
	root := request.Document().Root()
	switch id {
	case "openai-input-tokens":
		if err := known(root, "model", "input", "instructions", "parallel_tool_calls", "personality", "previous_response_id", "conversation", "reasoning", "text", "tool_choice", "tools", "truncation"); err != nil {
			return nil, err
		}
	case "anthropic-count-tokens":
		if err := known(root, "model", "messages", "system", "tools", "thinking", "tool_choice", "cache_control", "output_config"); err != nil {
			return nil, err
		}
	case "gemini-count-tokens":
		if err := known(root, "contents", "generateContentRequest"); err != nil {
			return nil, err
		}
		full := operations.Member(root, "generateContentRequest")
		if full.Kind() == oif.Object {
			if err := known(full, "model", "contents", "systemInstruction", "tools", "toolConfig", "safetySettings", "generationConfig", "cachedContent"); err != nil {
				return nil, err
			}
		}
	case "bedrock-count-tokens":
		if view.(CountRequest).scope == "invoke-model-body" {
			return nil, policyConflict("/input/invokeModel/body")
		}
		if err := known(root, "input"); err != nil {
			return nil, err
		}
		if err := known(operations.Member(operations.Member(root, "input"), "converse"), "messages", "system", "toolConfig", "additionalModelRequestFields"); err != nil {
			return nil, err
		}
		if value, present := operations.Member(operations.Member(root, "input"), "converse").Lookup("additionalModelRequestFields"); present && !operations.Optional(value) {
			return nil, policyConflict("/input/converse/additionalModelRequestFields")
		}
	}
	if err := policyShapes(root, id); err != nil {
		return nil, err
	}
	return collect(root, "", false)
}
func tokenizeText(request oif.Request) ([]operations.Text, error) {
	if _, err := tokenizeRequest(request); err != nil {
		return nil, err
	}
	root := request.Document().Root()
	if err := known(root, "inputs", "add_special_tokens", "prompt_name"); err != nil {
		return nil, err
	}
	if prompt, present := root.Lookup("prompt_name"); present && prompt.Kind() != oif.Null {
		return nil, policyConflict("/prompt_name")
	}
	return collect(operations.Member(root, "inputs"), "/inputs", false)
}
func outputText(result oif.Result) ([]operations.Text, error) {
	root := result.Source().Root()
	texts, err := collect(root, "", false)
	if err != nil {
		return nil, err
	}
	var keys func(oif.Value, string)
	keys = func(value oif.Value, path string) {
		if value.Kind() == oif.Object {
			for _, member := range value.Members() {
				p := operations.Pointer(path, member.Name)
				texts = append(texts, operations.Text{Pointer: p, Value: member.Name})
				keys(member.Value, p)
			}
		}
		if value.Kind() == oif.Array {
			for i, item := range value.Elements() {
				keys(item, path+"/"+strconv.Itoa(i))
			}
		}
	}
	keys(root, "")
	return texts, nil
}
