package operationplan

import (
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations"
)

func (p *Plan) CheckInput() ([]contentpolicy.Decision, error) {
	return p.CheckInputWithBudget(nil)
}

// CheckInputWithBudget lets inspection callers bound cumulative regexp work
// across plans. A nil callback leaves ordinary serving unchanged.
func (p *Plan) CheckInputWithBudget(reserve func(int) error) ([]contentpolicy.Decision, error) {
	policy := p.template.policy
	if policy != nil && len(policy.Output) > 0 && p.template.codec.OutputText == nil {
		return nil, fail("policy_conflict", "/content_policy", "inspectable_output", "This operation has no qualified output policy coverage.")
	}
	if policy == nil || len(policy.Input) == 0 {
		return nil, nil
	}
	if p.template.codec.InputText == nil {
		return nil, fail("policy_conflict", "/request", "inspectable_input", "The native operation has no qualified input policy coverage.")
	}
	texts, err := p.GuardrailInput()
	if err != nil {
		return nil, err
	}
	decisions := []contentpolicy.Decision{}
	for _, rule := range policy.Input {
		for _, text := range texts {
			if reserve != nil {
				if err := reserve(len(text.Value)); err != nil {
					return decisions, err
				}
			}
			if rule.Re.MatchString(text.Value) {
				decisions = append(decisions, contentpolicy.Decision{RuleID: rule.ID, Phase: contentpolicy.PhaseInput, Action: contentpolicy.ActionBlock, Outcome: contentpolicy.OutcomeBlocked})
				return decisions, fail("content_policy_blocked", "/request", "content_policy", "The effective request was blocked by its content policy.")
			}
		}
	}
	return decisions, nil
}
func (p *Plan) checkOutput(codec operations.Dialect, result oif.Result) ([]contentpolicy.Decision, error) {
	policy := p.template.policy
	if policy == nil || len(policy.Output) == 0 {
		return nil, nil
	}
	if codec.OutputText == nil {
		return nil, fail("policy_conflict", "/result", "inspectable_output", "The native operation has no qualified output policy coverage.")
	}
	texts, err := codec.OutputText(result)
	if err != nil {
		return nil, err
	}
	return checkOutputRules(policy, texts)
}
func checkOutputRules(policy *contentpolicy.Compiled, texts []operations.Text) ([]contentpolicy.Decision, error) {
	decisions := []contentpolicy.Decision{}
	for _, rule := range policy.Output {
		for _, text := range texts {
			if rule.Re.MatchString(text.Value) {
				decisions = append(decisions, contentpolicy.Decision{RuleID: rule.ID, Phase: contentpolicy.PhaseOutput, Action: contentpolicy.ActionBlock, Outcome: contentpolicy.OutcomeBlocked})
				return decisions, fail("content_policy_blocked", "/result", "content_policy", "The native result was blocked by its content policy.")
			}
		}
	}
	return decisions, nil
}

// GuardrailInput exposes the operation-owned inspection surface after defaults.
func (p *Plan) GuardrailInput() ([]operations.Text, error) {
	if p.template.codec.InputText == nil {
		return nil, fail("guardrail_surface_unavailable", "/request", "guardrail_input", "The operation has no text inspection contract.")
	}
	texts, err := p.template.codec.InputText(p.effective)
	if err != nil || p.mapping == nil {
		return texts, err
	}
	codec, _ := operationregistry.Lookup(p.source.Descriptor().Dialect.ID)
	if codec.InputText == nil {
		return nil, fail("guardrail_surface_unavailable", "/request", "guardrail_input", "The source operation has no text inspection contract.")
	}
	source, err := codec.InputText(p.source)
	return append(texts, source...), err
}

// GuardrailOutput uses the operation's own result contract. Vectors and other
// opaque payloads must never be mistaken for an empty, successfully inspected
// text response.
func (p *Plan) GuardrailOutput(result Result) ([]operations.Text, error) {
	if p.template.codec.OutputText == nil {
		return nil, fail("guardrail_surface_unavailable", "/result", "guardrail_output", "The operation has no text inspection contract.")
	}
	texts, err := p.template.codec.OutputText(result.Envelope)
	if err != nil || p.mapping == nil {
		return texts, err
	}
	codec, _ := operationregistry.Lookup(p.source.Descriptor().Dialect.ID)
	if codec.OutputText == nil {
		return nil, fail("guardrail_surface_unavailable", "/result", "guardrail_output", "The source operation has no text inspection contract.")
	}
	projected, err := codec.OutputText(result.projected)
	return append(texts, projected...), err
}
