package interaction

import (
	"slices"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/oif"
)

// CheckInput uses the same effective native input for runtime and no-inference
// inspection. It never rewrites the request and never includes values in its
// decisions/errors. Tool definitions and schemas are part of model input.
// Whether a native control is inspectable at all is the registered dialect's
// InputCoverage admission, run during Bind.
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
