package operationplan

import (
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

func (p *Plan) CheckInput() ([]contentpolicy.Decision, error) {
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
	texts, err := p.template.codec.InputText(p.effective)
	if err != nil {
		return nil, err
	}
	decisions := []contentpolicy.Decision{}
	for _, rule := range policy.Input {
		for _, text := range texts {
			if rule.Re.MatchString(text.Value) {
				decisions = append(decisions, contentpolicy.Decision{RuleID: rule.ID, Phase: contentpolicy.PhaseInput, Action: contentpolicy.ActionBlock, Outcome: contentpolicy.OutcomeBlocked})
				return decisions, fail("content_policy_blocked", "/request", "content_policy", "The effective request was blocked by its content policy.")
			}
		}
	}
	return decisions, nil
}
func (p *Plan) checkOutput(result oif.Result) ([]contentpolicy.Decision, error) {
	policy := p.template.policy
	if policy == nil || len(policy.Output) == 0 {
		return nil, nil
	}
	if p.template.codec.OutputText == nil {
		return nil, fail("policy_conflict", "/result", "inspectable_output", "The native operation has no qualified output policy coverage.")
	}
	texts, err := p.template.codec.OutputText(result)
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
