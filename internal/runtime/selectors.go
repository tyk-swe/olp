package runtime

import (
	"slices"

	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// PredicateResult is the verdict of a classifier or plugin predicate. A
// failed predicate does not match, so selection falls through to the next
// selector.
type PredicateResult struct {
	Matched bool
	// Label is the classifier's label, when it produced one.
	Label string
	// Failure names why the predicate could not be decided.
	Failure string
}

// SelectorOutcome explains one selector the planner evaluated.
type SelectorOutcome struct {
	ID      string  `json:"id"`
	Matched bool    `json:"matched"`
	Outcome string  `json:"outcome"`
	Label   *string `json:"label"`
}

// selection is the result of walking a route's selectors: the first that
// matched, if any, and the trace of every selector evaluated on the way.
type selection struct {
	selector *Selector
	trace    []SelectorOutcome
}

// selectTargets evaluates a route's selectors in order. Static predicates are
// decided first, so a classifier or plugin is consulted only for a selector
// whose features already match. The operation is the one being planned,
// whatever the features say.
func selectTargets(s *Snapshot, route Route, operation string, options SelectionOptions) selection {
	var out selection
	if len(route.Selectors) == 0 || options.Features == nil {
		return out
	}
	features := *options.Features
	features.Operation = operation
	for i := range route.Selectors {
		selector := &route.Selectors[i]
		outcome := evaluateSelector(s, selector, features, options)
		out.trace = append(out.trace, outcome)
		if outcome.Matched {
			out.selector = selector
			return out
		}
	}
	return out
}

func evaluateSelector(s *Snapshot, selector *Selector, features Features, options SelectionOptions) SelectorOutcome {
	out := SelectorOutcome{ID: selector.ID, Outcome: "not_matched"}
	if !selector.When.Matches(features) {
		return out
	}
	if !selector.When.Static() {
		if options.Evaluate == nil {
			out.Outcome = "predicate_unavailable"
			return out
		}
		result := options.Evaluate(*selector)
		if result.Label != "" {
			label := result.Label
			out.Label = &label
		}
		if result.Failure != "" {
			out.Outcome = result.Failure
			return out
		}
		if !result.Matched {
			return out
		}
	}
	if selector.Route != "" {
		next, ok := s.Routes[selector.Route]
		switch {
		case !ok || !slices.Contains(next.Operations, features.Operation):
			out.Outcome = "selector_route_unavailable"
			return out
		case options.Permitted != nil && !options.Permitted(next):
			out.Outcome = "selector_route_forbidden"
			return out
		}
	}
	out.Matched, out.Outcome = true, "matched"
	return out
}

// admits reports whether a selector keeps a target: one carrying any of the
// selector's tags. Without a matching selector every target stays.
func (sel selection) admits(target Target) bool {
	if sel.selector == nil || len(sel.selector.Tags) == 0 {
		return true
	}
	return slices.ContainsFunc(sel.selector.Tags, func(tag string) bool { return slices.Contains(target.Tags, tag) })
}

// DescribeRequest gives the features selectors test: the operation, the
// request's input estimate and reply bound, streaming and what the request
// asks for. Gateway planning and route simulation share it, so a selector
// sees the same request in both. Attribution labels never take part.
func DescribeRequest(operation string, request *openai.Request, demand *TokenDemand) *Features {
	features := &Features{Operation: operation}
	if demand != nil {
		features.InputTokens, features.OutputTokens = demand.EstimatedInputTokens, demand.MaxOutputTokens
	}
	if request == nil {
		return features
	}
	described := protocols.RequestFeatures(request)
	features.Streaming, features.Tools = request.Stream, described.Tools
	features.Modalities, features.StructuredOutput = described.Modalities, described.StructuredOutput
	features.ReasoningEffort = described.ReasoningEffort
	return features
}
