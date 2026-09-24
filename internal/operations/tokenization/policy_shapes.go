package tokenization

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
	"slices"
	"strconv"
)

// Policy coverage is narrower than native identity. Unknown native nodes stay
// in requests without an applicable policy; a text policy cannot infer that an
// unknown encoded input or provider-owned tool prompt is inspectable text.
func policyShapes(root oif.Value, id string) error {
	switch id {
	case "openai-input-tokens":
		if err := openAIItems(operations.Member(root, "input"), "/input"); err != nil {
			return err
		}
		if tools, present := root.Lookup("tools"); present && tools.Kind() == oif.Array {
			for i, tool := range tools.Elements() {
				if operations.String(operations.Member(tool, "type")) != "function" {
					return policyConflict("/tools/" + strconv.Itoa(i))
				}
				if err := known(tool, "type", "name", "description", "parameters", "strict"); err != nil {
					return err
				}
			}
		}
		if reasoning, present := root.Lookup("reasoning"); present && reasoning.Kind() == oif.Object {
			if err := known(reasoning, "effort", "summary", "generate_summary"); err != nil {
				return err
			}
		}
	case "anthropic-count-tokens":
		for i, message := range operations.Member(root, "messages").Elements() {
			if err := known(message, "role", "content"); err != nil {
				return err
			}
			if err := anthropicContent(operations.Member(message, "content"), "/messages/"+strconv.Itoa(i)+"/content"); err != nil {
				return err
			}
		}
		if err := anthropicContent(operations.Member(root, "system"), "/system"); err != nil {
			return err
		}
		for i, tool := range operations.Member(root, "tools").Elements() {
			kind := operations.Member(tool, "type")
			if kind.Kind() != oif.Absent && operations.String(kind) != "custom" {
				return policyConflict("/tools/" + strconv.Itoa(i))
			}
			if err := known(tool, "type", "name", "description", "input_schema", "cache_control", "strict", "defer_loading"); err != nil {
				return err
			}
		}
	case "gemini-count-tokens":
		contentRoot := root
		if full := operations.Member(root, "generateContentRequest"); full.Kind() == oif.Object {
			contentRoot = full
		}
		for i, content := range operations.Member(contentRoot, "contents").Elements() {
			if err := geminiPolicyContent(content, "/contents/"+strconv.Itoa(i)); err != nil {
				return err
			}
		}
		if system, present := contentRoot.Lookup("systemInstruction"); present && !operations.Optional(system) {
			if err := geminiPolicyContent(system, "/systemInstruction"); err != nil {
				return err
			}
		}
		for i, tool := range operations.Member(contentRoot, "tools").Elements() {
			if err := known(tool, "functionDeclarations"); err != nil {
				return err
			}
			if _, present := tool.Lookup("functionDeclarations"); !present {
				return policyConflict("/tools/" + strconv.Itoa(i))
			}
		}
	case "bedrock-count-tokens":
		converse := operations.Member(operations.Member(root, "input"), "converse")
		for i, message := range operations.Member(converse, "messages").Elements() {
			if err := known(message, "role", "content"); err != nil {
				return err
			}
			if err := bedrockContent(operations.Member(message, "content"), "/input/converse/messages/"+strconv.Itoa(i)+"/content"); err != nil {
				return err
			}
		}
		if system, present := converse.Lookup("system"); present {
			if err := bedrockContent(system, "/input/converse/system"); err != nil {
				return err
			}
		}
	}
	return nil
}
func openAIItems(input oif.Value, path string) error {
	if input.Kind() == oif.String || operations.Optional(input) {
		return nil
	}
	for i, item := range input.Elements() {
		p := path + "/" + strconv.Itoa(i)
		kind := operations.String(operations.Member(item, "type"))
		switch kind {
		case "", "message":
			if err := known(item, "type", "role", "content", "id", "status", "phase"); err != nil {
				return err
			}
			content := operations.Member(item, "content")
			if content.Kind() == oif.String {
				continue
			}
			for j, block := range content.Elements() {
				if !slices.Contains([]string{"input_text", "output_text", "text", "refusal"}, operations.String(operations.Member(block, "type"))) {
					return policyConflict(p + "/content/" + strconv.Itoa(j))
				}
				if err := known(block, "type", "text", "refusal", "annotations"); err != nil {
					return err
				}
			}
		case "function_call":
			if err := known(item, "type", "name", "call_id", "arguments", "id", "status"); err != nil {
				return err
			}
		case "function_call_output":
			if err := known(item, "type", "call_id", "output", "id", "status"); err != nil {
				return err
			}
			output := operations.Member(item, "output")
			if output.Kind() != oif.String {
				return policyConflict(p + "/output")
			}
		case "reasoning":
			if err := known(item, "type", "id", "summary", "status"); err != nil {
				return err
			}
			for _, summary := range operations.Member(item, "summary").Elements() {
				if operations.String(operations.Member(summary, "type")) != "summary_text" {
					return policyConflict(p + "/summary")
				}
				if err := known(summary, "type", "text"); err != nil {
					return err
				}
			}
		default:
			return policyConflict(p)
		}
	}
	return nil
}
func anthropicContent(content oif.Value, path string) error {
	if content.Kind() == oif.String || content.Kind() == oif.Absent {
		return nil
	}
	for i, block := range content.Elements() {
		p := path + "/" + strconv.Itoa(i)
		switch operations.String(operations.Member(block, "type")) {
		case "text":
			if err := known(block, "type", "text", "cache_control", "citations"); err != nil {
				return err
			}
		case "thinking":
			if err := known(block, "type", "thinking", "signature"); err != nil {
				return err
			}
		case "tool_use":
			if err := known(block, "type", "id", "name", "input", "cache_control"); err != nil {
				return err
			}
		case "tool_result":
			if err := known(block, "type", "tool_use_id", "content", "is_error", "cache_control"); err != nil {
				return err
			}
			if err := anthropicContent(operations.Member(block, "content"), p+"/content"); err != nil {
				return err
			}
		default:
			return policyConflict(p)
		}
	}
	return nil
}
func geminiPolicyContent(content oif.Value, path string) error {
	if err := known(content, "role", "parts"); err != nil {
		return err
	}
	for i, part := range operations.Member(content, "parts").Elements() {
		p := path + "/parts/" + strconv.Itoa(i)
		if err := known(part, "text", "thought", "functionCall", "functionResponse"); err != nil {
			return err
		}
		if call, present := part.Lookup("functionCall"); present {
			if err := known(call, "name", "args", "id"); err != nil {
				return err
			}
		}
		if response, present := part.Lookup("functionResponse"); present {
			if err := known(response, "name", "response", "id"); err != nil {
				return err
			}
		}
		if _, text := part.Lookup("text"); !text {
			_, call := part.Lookup("functionCall")
			_, response := part.Lookup("functionResponse")
			if !call && !response {
				return policyConflict(p)
			}
		}
	}
	return nil
}
func bedrockContent(content oif.Value, path string) error {
	for i, block := range content.Elements() {
		p := path + "/" + strconv.Itoa(i)
		if len(block.Members()) != 1 {
			return policyConflict(p)
		}
		member := block.Members()[0]
		switch member.Name {
		case "text":
			if member.Value.Kind() != oif.String {
				return policyConflict(p)
			}
		case "cachePoint":
			if err := known(member.Value, "type", "ttl"); err != nil {
				return err
			}
		case "toolUse":
			if err := known(member.Value, "toolUseId", "name", "input", "type"); err != nil {
				return err
			}
		case "toolResult":
			if err := known(member.Value, "toolUseId", "content", "status", "type"); err != nil {
				return err
			}
			for _, item := range operations.Member(member.Value, "content").Elements() {
				if err := known(item, "text", "json"); err != nil {
					return err
				}
			}
		default:
			return policyConflict(p)
		}
	}
	return nil
}
