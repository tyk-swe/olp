package vendor

import (
	"cmp"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

func aporia(ctx context.Context, x exchange, r Request) (Decision, error) {
	body := map[string]any{"messages": messages(r), "validation_target": "prompt"}
	if r.Output {
		body = map[string]any{"response": r.Text, "validation_target": "response"}
	}
	raw, err := x.keyed(ctx, body, "X-APORIA-API-KEY", "")
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Decision{}, errUnavailable
	}
	switch response.Action {
	case "passthrough":
		return Decision{}, nil
	case "block":
		return flag("aporia.policy"), nil
	default:
		return Decision{}, errUnavailable
	}
}

type dynamoParameters struct {
	ModelID   string   `json:"model_id,omitempty"`
	PolicyIDs []string `json:"policy_ids,omitempty"`
}

func (p dynamoParameters) valid() bool { return optional(p.ModelID) && names(p.PolicyIDs, 256) }

func dynamo(ctx context.Context, x exchange, p dynamoParameters, r Request) (Decision, error) {
	body := map[string]any{"messages": messages(r)}
	if p.ModelID != "" {
		body["modelId"] = p.ModelID
	}
	if len(p.PolicyIDs) > 0 {
		body["policyIds"] = p.PolicyIDs
	}
	raw, err := x.bearer(ctx, body)
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		FinalAction string `json:"finalAction"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Decision{}, errUnavailable
	}
	switch response.FinalAction {
	case "NONE":
		return Decision{}, nil
	case "BLOCK", "WARN":
		return flag("dynamoai.policy"), nil
	default:
		return Decision{}, errUnavailable
	}
}

func aim(ctx context.Context, x exchange, r Request) (Decision, error) {
	return litellmHook(ctx, x, r, "aim")
}

func cato(ctx context.Context, x exchange, r Request) (Decision, error) {
	return litellmHook(ctx, x, r, "cato")
}

func litellmHook(ctx context.Context, x exchange, r Request, name string) (Decision, error) {
	raw, err := x.withKey(ctx, map[string]any{"messages": messages(r)}, func(h http.Header, key string) {
		h.Set("Authorization", "Bearer "+key)
		h.Set("x-"+name+"-litellm-hook", pick(r, "pre_call", "post_call"))
	})
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Required *struct {
			Action *string `json:"action_type"`
		} `json:"required_action"`
		Analysis json.RawMessage `json:"analysis_result"`
		Redacted *struct {
			Messages []content `json:"all_redacted_messages"`
		} `json:"redacted_chat"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Decision{}, errUnavailable
	}
	category := name + ".policy"
	if response.Required == nil || response.Required.Action == nil {
		var analysis map[string]json.RawMessage
		if json.Unmarshal(response.Analysis, &analysis) != nil || analysis == nil {
			return Decision{}, errUnavailable
		}
		return Decision{}, nil
	}
	switch *response.Required.Action {
	case "monitor_action", "block_action":
		return flag(category), nil
	case "anonymize_action":
		if response.Redacted == nil {
			return Decision{}, errUnavailable
		}
		return replaceOnly(response.Redacted.Messages, r, category)
	default:
		return Decision{}, errUnavailable
	}
}

type grayswanParameters struct {
	Threshold     *float64          `json:"violation_threshold,omitempty"`
	PolicyID      string            `json:"policy_id,omitempty"`
	ReasoningMode string            `json:"reasoning_mode,omitempty"`
	Categories    map[string]string `json:"categories,omitempty"`
}

func (p grayswanParameters) valid() bool {
	if !optional(p.PolicyID) || len(p.Categories) > 32 || (p.Threshold != nil && !unit(*p.Threshold)) {
		return false
	}
	switch p.ReasoningMode {
	case "", "off", "hybrid", "thinking":
	default:
		return false
	}
	for name, description := range p.Categories {
		if !optional(name) || len(description) > 4096 {
			return false
		}
	}
	return true
}

func grayswan(ctx context.Context, x exchange, p grayswanParameters, r Request) (Decision, error) {
	threshold := 0.5
	if p.Threshold != nil {
		threshold = *p.Threshold
	}
	body := map[string]any{"messages": messages(r)}
	if p.PolicyID != "" {
		body["policy_id"] = p.PolicyID
	}
	if p.ReasoningMode != "" {
		body["reasoning_mode"] = p.ReasoningMode
	}
	if len(p.Categories) > 0 {
		body["categories"] = p.Categories
	}
	raw, err := x.withKey(ctx, body, func(h http.Header, key string) {
		h.Set("Authorization", "Bearer "+key)
		h.Set("grayswan-api-key", key)
	})
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Violation *float64 `json:"violation"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Violation == nil || !unit(*response.Violation) {
		return Decision{}, errUnavailable
	}
	return flagIf(*response.Violation >= threshold, "grayswan.policy"), nil
}

func unit(value float64) bool { return !math.IsNaN(value) && value >= 0 && value <= 1 }

type ciscoRule struct {
	RuleName       string   `json:"rule_name"`
	RuleID         string   `json:"rule_id,omitempty"`
	EntityTypes    []string `json:"entity_types,omitempty"`
	Classification string   `json:"classification,omitempty"`
}

func (r ciscoRule) valid() bool {
	return (r.RuleName != "" || r.RuleID != "") && label(r.RuleName) && label(r.RuleID) && label(r.Classification) && labels(r.EntityTypes)
}

type ciscoParameters struct {
	ProfileID       string      `json:"integration_profile_id,omitempty"`
	ProfileVersion  string      `json:"integration_profile_version,omitempty"`
	TenantID        string      `json:"integration_tenant_id,omitempty"`
	IntegrationType string      `json:"integration_type,omitempty"`
	EnabledRules    []ciscoRule `json:"enabled_rules,omitempty"`
}

func (p ciscoParameters) valid() bool {
	return label(p.ProfileID) && label(p.ProfileVersion) && label(p.TenantID) && label(p.IntegrationType) && len(p.EnabledRules) <= 64 && !slices.ContainsFunc(p.EnabledRules, func(r ciscoRule) bool { return !r.valid() })
}

func cisco(ctx context.Context, x exchange, p ciscoParameters, r Request) (Decision, error) {
	raw, err := x.keyed(ctx, map[string]any{"messages": messages(r), "metadata": map[string]string{}, "config": p}, "X-Cisco-AI-Defense-API-Key", "")
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Action         string          `json:"action"`
		Safe           *bool           `json:"is_safe"`
		Sanitized      *string         `json:"sanitized_text"`
		CamelSanitized *string         `json:"sanitizedText"`
		Error          json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &response) != nil || failed(response.Error) {
		return Decision{}, errUnavailable
	}
	switch strings.ToLower(strings.TrimSpace(response.Action)) {
	case "deny", "denied", "block", "blocked", "reject", "rejected", "unsafe", "malicious":
		return flag("cisco_ai_defense.policy"), nil
	case "allow", "allowed", "safe", "ok":
		return Decision{}, nil
	case "redact", "redacted", "sanitize", "sanitized", "mask":
		replacement := cmp.Or(response.Sanitized, response.CamelSanitized)
		if replacement == nil {
			return Decision{}, errUnavailable
		}
		return replace(r, *replacement, "cisco_ai_defense.policy")
	case "":
		if response.Safe == nil {
			return Decision{}, errUnavailable
		}
		return flagIf(!*response.Safe, "cisco_ai_defense.policy"), nil
	default:
		return Decision{}, errUnavailable
	}
}

func promptGuard(ctx context.Context, x exchange, r Request) (Decision, error) {
	raw, err := x.keyed(ctx, map[string]any{"messages": messages(r), "direction": pick(r, "input", "output")}, "X-API-Key", "")
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Decision string         `json:"decision"`
		Messages *[]roleContent `json:"redacted_messages"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Decision{}, errUnavailable
	}
	if response.Decision == "redact" {
		return replaceMessage(response.Messages, r, "promptguard.policy")
	}
	return allowBlock(response.Decision, "promptguard.policy")
}

type xecguardParameters struct {
	Model       string   `json:"model,omitempty"`
	PolicyNames []string `json:"policy_names,omitempty"`
}

func (p xecguardParameters) valid() bool { return label(p.Model) && labels(p.PolicyNames) }

var xecguardDefaultPolicies = []string{"Default_Policy_SystemPromptEnforcement", "Default_Policy_HarmfulContentProtection", "Default_Policy_GeneralPromptAttackProtection"}

func xecguard(ctx context.Context, x exchange, p xecguardParameters, r Request) (Decision, error) {
	policies := p.PolicyNames
	if len(policies) == 0 {
		policies = xecguardDefaultPolicies
	}
	body := map[string]any{"model": cmp.Or(p.Model, "xecguard_v2"), "scan_type": pick(r, "input", "response"), "messages": messages(r), "policy_names": policies}
	raw, err := x.bearer(ctx, body)
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Decision string `json:"decision"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Decision{}, errUnavailable
	}
	switch response.Decision {
	case "SAFE":
		return Decision{}, nil
	case "UNSAFE":
		return flag("xecguard.policy"), nil
	default:
		return Decision{}, errUnavailable
	}
}

type qualifireParameters struct {
	EvaluationID           string   `json:"evaluation_id,omitempty"`
	Assertions             []string `json:"assertions,omitempty"`
	PromptInjections       bool     `json:"prompt_injections,omitempty"`
	HallucinationsCheck    bool     `json:"hallucinations_check,omitempty"`
	GroundingCheck         bool     `json:"grounding_check,omitempty"`
	PIICheck               bool     `json:"pii_check,omitempty"`
	ContentModerationCheck bool     `json:"content_moderation_check,omitempty"`
}

func (p qualifireParameters) valid() bool {
	checks := p.EvaluationID != "" || len(p.Assertions) > 0 || p.PromptInjections || p.HallucinationsCheck || p.GroundingCheck || p.PIICheck || p.ContentModerationCheck
	return checks && len(p.EvaluationID) <= 256 && len(p.Assertions) <= 64 && !slices.ContainsFunc(p.Assertions, func(a string) bool { return a == "" || len(a) > 8192 })
}

type qualifireRequest struct {
	qualifireParameters
	Messages []map[string]string `json:"messages"`
	Input    *string             `json:"input,omitempty"`
	Output   *string             `json:"output,omitempty"`
}

func qualifireBody(p qualifireParameters, r Request) qualifireRequest {
	text, empty := r.Text, ""
	evaluation := p.EvaluationID != ""
	switch {
	case r.Output && evaluation:
		return qualifireRequest{qualifireParameters: p, Messages: []map[string]string{}, Input: &empty, Output: &text}
	case r.Output:
		return qualifireRequest{qualifireParameters: p, Messages: []map[string]string{}, Output: &text}
	case evaluation:
		return qualifireRequest{qualifireParameters: p, Messages: messages(r), Input: &text, Output: &empty}
	default:
		return qualifireRequest{qualifireParameters: p, Messages: messages(r)}
	}
}

func qualifire(ctx context.Context, x exchange, p qualifireParameters, r Request) (Decision, error) {
	raw, err := x.keyed(ctx, qualifireBody(p, r), "X-Qualifire-API-Key", "")
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		EvaluationResults *[]struct {
			Results *[]struct {
				Flagged *bool `json:"flagged"`
			} `json:"results"`
		} `json:"evaluationResults"`
	}
	if json.Unmarshal(raw, &response) != nil || response.EvaluationResults == nil || len(*response.EvaluationResults) == 0 {
		return Decision{}, errUnavailable
	}
	flagged := false
	for _, evaluation := range *response.EvaluationResults {
		if evaluation.Results == nil || len(*evaluation.Results) == 0 {
			return Decision{}, errUnavailable
		}
		for _, result := range *evaluation.Results {
			if result.Flagged == nil {
				return Decision{}, errUnavailable
			}
			flagged = flagged || *result.Flagged
		}
	}
	return flagIf(flagged, "qualifire.policy"), nil
}

func rubrik(ctx context.Context, x exchange, r Request) (Decision, error) {
	body := map[string]any{"model": "", "messages": messages(r)}
	if r.Output {
		completion := map[string]any{"id": uuid.NewString(), "object": "chat.completion", "created": time.Now().Unix(), "model": "", "choices": []any{map[string]any{"index": 0, "message": message(r), "finish_reason": "stop"}}}
		body = map[string]any{"request": map[string]any{}, "response": completion}
	}
	raw, err := x.bearer(ctx, body)
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Choices *[]struct {
			Message *content `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Decision{}, errUnavailable
	}
	if !r.Output && response.Choices == nil {
		var allowed map[string]json.RawMessage
		if json.Unmarshal(raw, &allowed) != nil || allowed == nil {
			return Decision{}, errUnavailable
		}
		return Decision{}, nil
	}
	if response.Choices == nil || len(*response.Choices) != 1 || (*response.Choices)[0].Message == nil || (*response.Choices)[0].Message.Content == nil {
		return Decision{}, errUnavailable
	}
	return flagIf(!r.Output || *(*response.Choices)[0].Message.Content != r.Text, "rubrik.policy"), nil
}

type singulrParameters struct {
	ApplicationID string `json:"application_id,omitempty"`
	GuardrailID   string `json:"guardrail_id,omitempty"`
}

func (p singulrParameters) valid() bool { return optional(p.ApplicationID) && optional(p.GuardrailID) }

func singulr(ctx context.Context, x exchange, p singulrParameters, r Request) (Decision, error) {
	body := map[string]any{"guardrail_scope": direction(r), "messages": messages(r)}
	if r.Output {
		body = map[string]any{"guardrail_scope": direction(r), "response": message(r)}
	}
	raw, err := x.withKey(ctx, body, func(h http.Header, key string) {
		h.Set("X-Singulr-Gateway-Token", key)
		if p.ApplicationID != "" {
			h.Set("X-Singulr-Enforcement-Entity-Id", p.ApplicationID)
		}
		if p.GuardrailID != "" {
			h.Set("X-Singulr-Guardrail-Id", p.GuardrailID)
		}
	})
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Block *bool `json:"should_block"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Block == nil {
		return Decision{}, errUnavailable
	}
	return flagIf(*response.Block, "singulr.policy"), nil
}

type headroomParameters struct {
	Model string `json:"model,omitempty"`
}

func (headroomParameters) valid() bool { return true }

func headroom(ctx context.Context, x exchange, p headroomParameters, r Request) (Decision, error) {
	body := map[string]any{"messages": messages(r)}
	if p.Model != "" {
		body["model"] = p.Model
	}
	raw, err := x.bearer(ctx, body)
	if err != nil {
		return Decision{}, err
	}
	var response struct {
		Messages []roleContent `json:"messages"`
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Messages) != 1 || response.Messages[0].Role != string(r.Role) {
		return Decision{}, errUnavailable
	}
	return compressed(r, response.Messages[0].Content, "headroom.compression")
}

func compressed(r Request, text *string, category string) (Decision, error) {
	if text == nil {
		return Decision{}, errUnavailable
	}
	if *text == r.Text {
		return Decision{}, nil
	}
	return replace(r, *text, category)
}
