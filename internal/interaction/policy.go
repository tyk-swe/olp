package interaction

import (
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func inputFields(wire openai.Family) []string {
	common := "model stream tools tool_choice temperature top_p top_k max_tokens metadata"
	switch wire {
	case openai.FamilyChat:
		return strings.Fields(common + " messages max_completion_tokens stream_options stop n presence_penalty frequency_penalty logit_bias seed response_format parallel_tool_calls logprobs top_logprobs user service_tier store")
	case openai.FamilyResponses:
		return strings.Fields(common + " input instructions max_output_tokens text parallel_tool_calls truncation service_tier store previous_response_id conversation background include")
	case openai.FamilyAnthropic:
		return strings.Fields(common + " messages system stop_sequences anthropic_version")
	case openai.FamilyGemini:
		return strings.Fields("contents systemInstruction generationConfig tools toolConfig safetySettings cachedContent")
	case openai.FamilyBedrock:
		return strings.Fields("messages system inferenceConfig toolConfig guardrailConfig additionalModelResponseFieldPaths requestMetadata performanceConfig")
	}
	return nil
}

func checkInputCoverage(wire openai.Family, document oif.Document) error {
	for _, field := range document.Root().Members() {
		if !slices.Contains(inputFields(wire), field.Name) {
			return incompatible("policy_conflict", safeField(field.Name), "input_policy_coverage", "The input policy cannot inspect an unknown or opaque native control.")
		}
	}
	for name, allowed := range map[string]string{
		"generationConfig": "temperature topP topK candidateCount maxOutputTokens stopSequences presencePenalty frequencyPenalty seed responseMimeType responseSchema responseJsonSchema responseLogprobs logprobs",
		"inferenceConfig":  "maxTokens temperature topP stopSequences",
		"stream_options":   "include_usage include_obfuscation",
	} {
		if value, present := document.Root().Lookup(name); present && value.Kind() != oif.Null && !onlyMembers(value, allowed) {
			return incompatible("policy_conflict", "/"+name, "input_policy_coverage", "A native control contains a member without a policy inspection contract.")
		}
	}
	if format, present := document.Root().Lookup("response_format"); present && format.Kind() != oif.Null && !inspectableFormat(format, false) {
		return incompatible("policy_conflict", "/response_format", "input_policy_coverage", "The response format has no complete policy inspection contract.")
	}
	if text, present := document.Root().Lookup("text"); present && text.Kind() != oif.Null {
		if !onlyMembers(text, "format verbosity") {
			return incompatible("policy_conflict", "/text", "input_policy_coverage", "The text format contains an unknown native control.")
		}
		if format, present := text.Lookup("format"); present && !inspectableFormat(format, true) {
			return incompatible("policy_conflict", "/text/format", "input_policy_coverage", "The text schema has no complete policy inspection contract.")
		}
	}
	// Tool schemas and argument/result JSON are declared data, not opaque native
	// state. Outside those positions, unknown message/part members fail closed.
	for _, name := range []string{"messages", "contents", "input", "system", "systemInstruction"} {
		if value, present := document.Root().Lookup(name); present {
			if err := inspectableContent(value, name == "input" && wire == openai.FamilyResponses); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{"tools", "toolConfig"} {
		if value, present := document.Root().Lookup(name); present && !inspectableTools(wire, name, value) {
			return incompatible("policy_conflict", "/"+name, "input_policy_coverage", "The input policy has no inspection contract for this native tool definition.")
		}
	}
	return nil
}
func inspectableContent(value oif.Value, responseInput bool) error {
	switch value.Kind() {
	case oif.Absent, oif.Null, oif.String:
		return nil
	case oif.Array:
		for _, element := range value.Elements() {
			if err := inspectableContent(element, responseInput); err != nil {
				return err
			}
		}
	case oif.Object:
		for _, field := range value.Members() {
			switch field.Name {
			case "role", "type", "name", "id", "tool_call_id", "tool_use_id", "is_error":
			case "text", "refusal", "content", "parts":
				if err := inspectableContent(field.Value, responseInput); err != nil {
					return err
				}
			case "tool_calls", "function_call", "functionCall", "functionResponse", "toolUse", "toolResult", "input", "output":
				if hasOpaqueNative(field.Value) {
					return incompatible("policy_conflict", "/messages", "input_policy_coverage", "The input includes opaque native tool or reasoning state.")
				}
			case "status", "action", "sources", "results", "annotations":
				// Replayed hosted-tool observations are declared data; their
				// queries, URLs and titles are all inspectable strings.
				if hasOpaqueNative(field.Value) {
					return incompatible("policy_conflict", "/messages", "input_policy_coverage", "The input includes opaque native tool or reasoning state.")
				}
			default:
				return incompatible("policy_conflict", "/messages", "input_policy_coverage", "The input policy has no inspection contract for a native message member.")
			}
		}
		kind := valueText(member(value, "type"))
		if kind != "" && !slices.Contains([]string{"text", "input_text", "output_text", "message", "tool_use", "tool_result", "function_call", "function_call_output", "refusal", "web_search_call"}, kind) {
			return incompatible("policy_conflict", "/messages", "input_policy_coverage", "The input policy cannot inspect this native content type.")
		}
	default:
		return incompatible("policy_conflict", "/messages", "input_policy_coverage", "The input policy requires inspectable native content.")
	}
	return nil
}
func hasOpaqueNative(value oif.Value) bool {
	for _, field := range value.Members() {
		if slices.Contains([]string{"signature", "thoughtSignature", "thought_signature", "encrypted_content", "redacted_thinking", "inlineData", "inline_data", "fileData", "image_url", "input_audio", "document", "bytes"}, field.Name) {
			return true
		}
		if hasOpaqueNative(field.Value) {
			return true
		}
	}
	for _, element := range value.Elements() {
		if hasOpaqueNative(element) {
			return true
		}
	}
	return false
}

// CheckInput uses the same effective native input for runtime and no-inference
// inspection. It never rewrites the request and never includes values in its
// decisions/errors. Tool definitions and schemas are part of model input.
func (p *Plan) CheckInput() ([]contentpolicy.Decision, error) {
	if p.template.policy == nil || len(p.template.policy.Input) == 0 {
		return nil, nil
	}
	texts := []string{}
	for _, name := range []string{"messages", "input", "contents", "system", "systemInstruction", "instructions", "tools", "toolConfig", "tool_choice", "response_format", "text", "generationConfig"} {
		if value, present := p.effective.Root().Lookup(name); present {
			collectPolicyText(value, &texts, name == "tools" || name == "toolConfig" || name == "response_format" || name == "text" || name == "generationConfig")
		}
	}
	decisions := make([]contentpolicy.Decision, 0, len(p.template.policy.Input))
	for _, rule := range p.template.policy.Input {
		decision := contentpolicy.Decision{RuleID: rule.ID, Phase: contentpolicy.PhaseInput, Action: contentpolicy.ActionBlock, Outcome: contentpolicy.OutcomePassed}
		for _, text := range texts {
			if rule.Re.MatchString(text) {
				decision.Outcome = contentpolicy.OutcomeBlocked
				decisions = append(decisions, decision)
				return decisions, incompatible("content_policy_blocked", "/request", "effective_input_policy", "The effective native request was blocked by the route content policy.")
			}
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
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
			if !schema && slices.Contains([]string{"role", "type", "id", "tool_call_id", "tool_use_id"}, field.Name) {
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
			collectPolicyText(field.Value, texts, schema || field.Name == "parameters" || field.Name == "input_schema")
		}
	}
}

func inspectableTools(wire openai.Family, name string, value oif.Value) bool {
	if value.Kind() == oif.Null {
		return true
	}
	if name == "toolConfig" {
		switch wire {
		case openai.FamilyGemini:
			return onlyMembers(value, "functionCallingConfig") && onlyMembers(member(value, "functionCallingConfig"), "mode allowedFunctionNames streamFunctionCallArguments")
		case openai.FamilyBedrock:
			if !onlyMembers(value, "tools toolChoice") {
				return false
			}
			value = member(value, "tools")
		default:
			return false
		}
	}
	if value.Kind() != oif.Array {
		return false
	}
	for _, tool := range value.Elements() {
		switch wire {
		case openai.FamilyChat:
			if !onlyMembers(tool, "type function") || valueText(member(tool, "type")) != "function" || !onlyMembers(member(tool, "function"), "name description parameters strict") {
				return false
			}
		case openai.FamilyResponses:
			kind := valueText(member(tool, "type"))
			if hostedRequestTools[kind] == "web_search" {
				// Hosted tool declarations are inspectable control data; deep
				// validation happens at admission against the hosted contract.
				if !onlyMembers(tool, "type filters search_context_size user_location") {
					return false
				}
				continue
			}
			if !onlyMembers(tool, "type name description parameters strict") || kind != "function" {
				return false
			}
		case openai.FamilyAnthropic:
			if !onlyMembers(tool, "name description input_schema cache_control") || member(tool, "name").Kind() != oif.String {
				return false
			}
		case openai.FamilyGemini:
			if !onlyMembers(tool, "functionDeclarations") {
				return false
			}
			declarations := member(tool, "functionDeclarations")
			if declarations.Kind() != oif.Array {
				return false
			}
			for _, function := range declarations.Elements() {
				if !onlyMembers(function, "name description parameters parametersJsonSchema") {
					return false
				}
			}
		case openai.FamilyBedrock:
			if !onlyMembers(tool, "toolSpec") || !onlyMembers(member(tool, "toolSpec"), "name description inputSchema") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func inspectableFormat(value oif.Value, responses bool) bool {
	if value.Kind() != oif.Object {
		return false
	}
	switch valueText(member(value, "type")) {
	case "text", "json_object":
		return onlyMembers(value, "type")
	case "json_schema":
		if responses {
			return onlyMembers(value, "type name description schema strict") && member(value, "schema").Kind() == oif.Object
		}
		return onlyMembers(value, "type json_schema") && onlyMembers(member(value, "json_schema"), "name description schema strict") && member(member(value, "json_schema"), "schema").Kind() == oif.Object
	}
	return false
}
