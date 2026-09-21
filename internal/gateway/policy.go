package gateway

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

func policyUnavailable(code, message string) *Error {
	return &Error{Status: http.StatusUnprocessableEntity, Type: "invalid_request_error", Code: code, Message: message}
}

func compiledPolicy(route *runtime.Route) (*contentpolicy.Compiled, *Error) {
	if route == nil || route.ContentPolicy == nil {
		return nil, nil
	}
	c, err := contentpolicy.Compile(route.ContentPolicy)
	if err != nil {
		return nil, serverError(http.StatusInternalServerError, "internal_error", "The route content policy could not be evaluated.")
	}
	return c, nil
}

func policySurfaceGate(route *runtime.Route) *Error {
	if route != nil && route.ContentPolicy != nil && len(route.ContentPolicy.Rules) > 0 {
		return policyUnavailable("content_policy_surface_unavailable", "The model `"+route.Slug+"` has a content policy this surface cannot enforce; use a canonical request surface.")
	}
	return nil
}

func recordDecision(x *execution, d contentpolicy.Decision) {
	for _, existing := range x.policyDecisions {
		if existing == d {
			return
		}
	}
	x.policyDecisions = append(x.policyDecisions, d)
}

func (s *Server) enforceContentPolicy(x *execution) *Error {
	compiled, e := compiledPolicy(x.route)
	if compiled == nil || e != nil {
		return e
	}
	if compiled.HasOutput() {
		if x.parsed.Stream {
			return policyUnavailable("content_policy_streaming_requires_unary", "The model `"+x.route.Slug+"` enforces an output content policy, which requires a buffered unary response; send the request without streaming.")
		}
		if x.providerState {
			return policyUnavailable("content_policy_surface_unavailable", "The model `"+x.route.Slug+"` enforces an output content policy that cannot be applied to provider-retained responses; send the request without stateful options.")
		}
	}
	for i := range compiled.Input {
		rule := &compiled.Input[i]
		matched, blocked := false, false
		transformed := protocols.InspectInputText(x.parsed, func(text string) (string, bool) {
			if !rule.Re.MatchString(text) {
				return text, false
			}
			matched = true
			if rule.Action == contentpolicy.ActionBlock {
				blocked = true
				return text, true
			}
			return rule.Re.ReplaceAllLiteralString(text, rule.Replacement), false
		})
		switch {
		case blocked:
			recordDecision(x, contentpolicy.Decision{RuleID: rule.ID, Phase: contentpolicy.PhaseInput, Action: contentpolicy.ActionBlock, Outcome: contentpolicy.OutcomeBlocked})
			return invalidRequest("content_policy_blocked", "The request was blocked by the route's content policy.", nil)
		case matched:
			x.parsed = transformed
			recordDecision(x, contentpolicy.Decision{RuleID: rule.ID, Phase: contentpolicy.PhaseInput, Action: contentpolicy.ActionRedact, Outcome: contentpolicy.OutcomeRedacted})
		}
	}
	return nil
}

func applySlotRules(x *execution, rules []contentpolicy.CompiledRule, phase string, value *string) bool {
	if value == nil {
		return false
	}
	for i := range rules {
		rule := &rules[i]
		if !rule.Re.MatchString(*value) {
			continue
		}
		if rule.Action == contentpolicy.ActionBlock {
			recordDecision(x, contentpolicy.Decision{RuleID: rule.ID, Phase: phase, Action: contentpolicy.ActionBlock, Outcome: contentpolicy.OutcomeBlocked})
			return true
		}
		*value = rule.Re.ReplaceAllLiteralString(*value, rule.Replacement)
		recordDecision(x, contentpolicy.Decision{RuleID: rule.ID, Phase: phase, Action: contentpolicy.ActionRedact, Outcome: contentpolicy.OutcomeRedacted})
	}
	return false
}

func (s *Server) enforceMediaInput(x *execution, request *media.Request) *Error {
	compiled, e := compiledPolicy(x.route)
	if compiled == nil || e != nil {
		return e
	}
	for _, slot := range []*string{&request.Prompt, &request.Input, request.TextPrompt, request.Instructions} {
		if applySlotRules(x, compiled.Input, contentpolicy.PhaseInput, slot) {
			return invalidRequest("content_policy_blocked", "The request was blocked by the route's content policy.", nil)
		}
	}
	return nil
}

func (s *Server) enforceCompletionOutput(x *execution, completion *openai.Completion) *Error {
	compiled, e := compiledPolicy(x.route)
	if compiled == nil || e != nil {
		return e
	}
	for _, slot := range []*string{&completion.OutputText, &completion.Refusal} {
		if applySlotRules(x, compiled.Output, contentpolicy.PhaseOutput, slot) {
			return invalidRequest("content_policy_blocked", "The response was blocked by the route's content policy.", nil)
		}
	}
	return nil
}

func (s *Server) enforceContentOutput(x *execution, body []byte) ([]byte, *Error) {
	compiled, e := compiledPolicy(x.route)
	if compiled == nil || e != nil {
		return body, e
	}
	current := body
	for i := range compiled.Output {
		rule := &compiled.Output[i]
		matched, blocked := false, false
		rewritten, err := protocols.InspectOutputText(x.family, current, func(text string) (string, bool) {
			if !rule.Re.MatchString(text) {
				return text, false
			}
			matched = true
			if rule.Action == contentpolicy.ActionBlock {
				blocked = true
				return text, true
			}
			return rule.Re.ReplaceAllLiteralString(text, rule.Replacement), false
		})
		if err != nil {
			return body, serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be inspected.")
		}
		switch {
		case blocked:
			recordDecision(x, contentpolicy.Decision{RuleID: rule.ID, Phase: contentpolicy.PhaseOutput, Action: contentpolicy.ActionBlock, Outcome: contentpolicy.OutcomeBlocked})
			return body, invalidRequest("content_policy_blocked", "The response was blocked by the route's content policy.", nil)
		case matched:
			current = rewritten
			recordDecision(x, contentpolicy.Decision{RuleID: rule.ID, Phase: contentpolicy.PhaseOutput, Action: contentpolicy.ActionRedact, Outcome: contentpolicy.OutcomeRedacted})
		}
	}
	return current, nil
}
