package gateway

import (
	"encoding/json"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/runtime"
)

type preparedProvider struct {
	invocation providerinvoke.Invocation
	// admitted is what the attempt is admitted under: the larger of the
	// caller's request and the one this provider is sent.
	admitted        admittedEstimate
	parameters      []string
	demand          *runtime.TokenDemand
	plan            *interaction.Plan
	policyDecisions []contentpolicy.Decision
}

// admittedEstimate is the estimate one attempt is admitted under. Reserve is the
// tokens held for it, the input and the largest reply the request allows. Input
// is the input alone, which is what an attempt records beside the usage its
// upstream reports, so that reports compare like with like; the reply bound is
// no estimate of anything the upstream will say.
type admittedEstimate struct {
	reserve    int64
	input      int64
	provenance estimate.Provenance
	family     estimate.Family
}

// A summary belongs to one source or one bound destination within an inference
// request. The same complete walk feeds routing and reservation, and each
// model family is counted once from it; the canonical parameter names still
// come from the effective request adapter.
type requestSummary struct {
	request    *openai.Request
	parameters []string
	prompt     *estimate.Prompt
}

func (x *execution) summarize(request *openai.Request) requestSummary {
	prompt := estimate.Walk(request)
	if x.request.counted != nil {
		prompt.Observe(x.request.counted)
	}
	return requestSummary{request: request, parameters: protocols.ParameterNames(request), prompt: prompt}
}

func (x *execution) summarizeSource() requestSummary {
	if x.sourceSummary == nil || x.sourceSummary.request != x.parsed {
		summary := x.summarize(x.parsed)
		x.sourceSummary = &summary
	}
	return *x.sourceSummary
}

// sourceDemand is what routing weighs a target's context window against: the
// caller's request as the model that would serve it counts it.
func (x *execution) sourceDemand(model string) *runtime.TokenDemand {
	return demandOf(x.summarizeSource().prompt.Estimate(estimate.ForModel(model), nil))
}

func demandOf(e estimate.Estimate) *runtime.TokenDemand {
	return &runtime.TokenDemand{EstimatedInputTokens: e.Input, MaxOutputTokens: e.Output}
}

func (x *execution) strict() bool {
	return x.route != nil && x.route.Fidelity.Strict()
}

// Prepared invocations are retained only for this inference request and bounded
// by its published route targets. Admission, estimation and dispatch consume the
// same effective defaults; accounting is still owned by the existing Attempt.
func (x *execution) preparedProvider(provider *runtime.Provider, model string) (preparedProvider, error) {
	key := provider.ID + "/" + provider.RevisionID + "/" + model
	if prepared, ok := x.preparedProviders[key]; ok {
		return prepared, nil
	}
	if x.strict() {
		var template *interaction.Template
		for _, target := range x.route.Targets {
			if target.ProviderID == provider.ID && target.ProviderModel == model {
				template, _ = x.snapshot().InteractionTemplate(x.route.Slug, target.ID)
				break
			}
		}
		if template == nil {
			return preparedProvider{}, &interaction.Error{Code: "target_capability", Requirement: "compiled_interaction", Message: "The selected target has no compiled strict interaction contract."}
		}
		binding := interaction.Context{Headers: x.semanticHeaders, Query: x.semanticQuery, AllowProviderState: x.authority.Policy.AllowProviderState, RequiredServing: x.serving, RetainedResponses: x.providerState}
		if x.continuation != nil {
			binding.ContinuationVersion = x.continuation.version
			binding.DurableContinuation = true
			binding.Continuation = x.continuation.prior
		}
		plan, err := template.Bind(x.parsed, binding)
		if err != nil {
			return preparedProvider{}, err
		}
		decisions, err := plan.CheckInput()
		effective := x.summarize(plan.EffectiveRequest())
		prepared := preparedProvider{plan: plan, invocation: providerinvoke.Invocation{Prepared: plan.Prepared(), Wire: plan.Wire()}, parameters: effective.parameters, policyDecisions: decisions}
		prepared.admitted, prepared.demand = x.preparedEstimate(model, effective, nil)
		if err != nil {
			return prepared, err
		}
		if x.preparedProviders == nil {
			x.preparedProviders = map[string]preparedProvider{}
		}
		x.preparedProviders[key] = prepared
		return prepared, nil
	}
	invocation, err := providerinvoke.Prepare(x.parsed, provider.Connector(), model, provider.ParameterDefaults)
	if err != nil {
		return preparedProvider{}, err
	}
	native := openai.NewSourceEnvelope(invocation.Wire, x.parsed.Route, x.parsed.Stream, invocation.Prepared.Document())
	var decisions []contentpolicy.Decision
	compiled, policyErr := compiledPolicy(x.route)
	if policyErr != nil {
		return preparedProvider{}, policyErr
	}
	if e := inputPolicyWireGate(x.route, compiled, invocation.Wire); e != nil {
		return preparedProvider{}, e
	}
	if compiled != nil {
		for _, rule := range compiled.Input {
			matched, blocked := false, false
			next, err := protocols.InspectInputText(native, func(text string) (string, bool) {
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
				return preparedProvider{}, policyUnavailable("content_policy_surface_unavailable", "The request could not be safely inspected by the route's content policy.")
			}
			if blocked {
				decisions = append(decisions, contentpolicy.Decision{RuleID: rule.ID, Phase: contentpolicy.PhaseInput, Action: contentpolicy.ActionBlock, Outcome: contentpolicy.OutcomeBlocked})
				return preparedProvider{policyDecisions: decisions}, invalidRequest("content_policy_blocked", "The request was blocked by the route's content policy.", nil)
			}
			if matched {
				native = next
				decisions = append(decisions, contentpolicy.Decision{RuleID: rule.ID, Phase: contentpolicy.PhaseInput, Action: contentpolicy.ActionRedact, Outcome: contentpolicy.OutcomeRedacted})
			}
		}
		if len(decisions) > 0 {
			prepared, err := oif.PrepareDestination(invocation.Prepared.Request(), invocation.Prepared.Descriptor(), native.OIF().Document(), oif.ExplicitTransform, "declared effective invocation content policy")
			if err != nil {
				return preparedProvider{}, err
			}
			invocation.Prepared = prepared.WithProvenance(invocation.Prepared.Provenance()...)
		}
	}
	effective := x.summarize(native)
	// Bedrock's native tool catalogue is outside the OpenAI tools field. Its
	// whole schema still contributes to the same conservative token reservation.
	var catalogue json.RawMessage
	if invocation.Wire == openai.FamilyBedrock {
		catalogue = native.Field("toolConfig")
	}
	prepared := preparedProvider{invocation: invocation, parameters: effective.parameters, policyDecisions: decisions}
	prepared.admitted, prepared.demand = x.preparedEstimate(model, effective, catalogue)
	if x.preparedProviders == nil {
		x.preparedProviders = map[string]preparedProvider{}
	}
	x.preparedProviders[key] = prepared
	return prepared, nil
}

// preparedEstimate prices the request for a target that is sent a request of its
// own, because a profile, a strict contract or a content policy rewrote it: the
// larger of the caller's request and that one, each counted for the family of
// the model it goes to. The reservation is the larger of the two; the input an
// attempt records is the larger input with the less trustworthy provenance of
// the two counts. When the provider's request reads the same as the caller's,
// which it usually does, the family is counted once for both.
func (x *execution) preparedEstimate(model string, effective requestSummary, catalogue json.RawMessage) (admittedEstimate, *runtime.TokenDemand) {
	source := x.summarizeSource()
	effective.prompt.Follow(source.prompt)
	counter := estimate.ForModel(model)
	from, to := source.prompt.Estimate(counter, nil), effective.prompt.Estimate(counter, nil)
	admitted := admittedEstimate{
		reserve:    max(from.Tokens(), to.Tokens()),
		input:      max(from.Input, to.Input),
		provenance: estimate.Weaker(from.Provenance, to.Provenance),
		family:     to.Family,
	}
	if text := estimate.SchemaText(catalogue); text != "" {
		tokens, provenance := counter.Count(text)
		admitted.reserve, admitted.input = addBounded(admitted.reserve, tokens), addBounded(admitted.input, tokens)
		// The text of a tool schema is counted as text, and the model reads it in
		// a rendering of its own, as the walker holds of every tool catalogue.
		admitted.provenance = estimate.Weaker(admitted.provenance, estimate.Weaker(provenance, estimate.ProvenanceCalibrated))
	}
	return admitted, demandOf(to)
}

// automatic reports that a provider is sent the caller's request with its own
// parameter defaults applied, and nothing a profile, a strict contract or a
// content policy would rewrite.
func (x *execution) automatic(provider *runtime.Provider) bool {
	return provider.ProfileID == "" && !x.strict() && (x.route == nil || x.route.ContentPolicy == nil)
}

// attemptEstimate prices the request for an attempt on one of a provider's
// models. The model decides how it is counted, so a route that mixes families
// reserves each target's own count, and a translated target is counted for the
// family it is sent to and not the dialect the caller spoke. Every count is
// made at most once per family for the request.
func (x *execution) attemptEstimate(provider *runtime.Provider, model string) (admittedEstimate, error) {
	if x.unary != nil {
		plan, err := x.unaryPlan(provider, model)
		if err != nil {
			return admittedEstimate{}, err
		}
		// The native operations charge four bytes per token over their own
		// documents, whatever model they are sent to.
		tokens := plan.Estimate()
		return admittedEstimate{reserve: tokens, input: tokens, provenance: estimate.ProvenanceHeuristic, family: estimate.FamilyOf(model)}, nil
	}
	if x.automatic(provider) {
		e := x.summarizeSource().prompt.Estimate(estimate.ForModel(model), provider.ParameterDefaults)
		return admittedEstimate{reserve: e.Tokens(), input: e.Input, provenance: e.Provenance, family: e.Family}, nil
	}
	prepared, err := x.preparedProvider(provider, model)
	if err != nil {
		return admittedEstimate{}, err
	}
	return prepared.admitted, nil
}

// attemptReservation is what the gate holds for one attempt.
func (x *execution) attemptReservation(a runtime.Attempt, provider *runtime.Provider) int64 {
	admitted, err := x.attemptEstimate(provider, a.UpstreamModel)
	if err != nil {
		return limits.MaxCounter
	}
	return max(admitted.reserve, 1)
}

// recordEstimate puts on an attempt the estimate it was admitted under, which
// is the input alone: the reservation also holds the reply the caller allowed,
// and that is a bound, not a guess at what the upstream will say. The model
// family is recorded whether or not an estimate was. Requests the gateway reads
// no prompt of, such as a stored-response lifecycle call, a realtime session
// or a job poll, record none.
func (x *execution) recordEstimate(fact *AttemptFact, a runtime.Attempt) {
	fact.ModelFamily = string(estimate.FamilyOf(a.UpstreamModel))
	if x.sizedInput != nil {
		fact.EstimatedInputTokens, fact.EstimateProvenance = *x.sizedInput, string(estimate.ProvenanceHeuristic)
		return
	}
	if x.parsed == nil && x.unary == nil {
		return
	}
	provider := x.snapshot().Providers[a.ProviderID]
	if admitted, err := x.attemptEstimate(&provider, a.UpstreamModel); err == nil {
		fact.EstimatedInputTokens, fact.EstimateProvenance = admitted.input, string(admitted.provenance)
	}
}

// inputPolicyWireGate refuses a destination wire the input rules cannot
// inspect, so an input policy never dispatches a body it has not seen.
func inputPolicyWireGate(route *runtime.Route, compiled *contentpolicy.Compiled, wire openai.Family) *Error {
	if compiled == nil || len(compiled.Input) == 0 || protocols.InputInspectable(wire) {
		return nil
	}
	return policyUnavailable("content_policy_surface_unavailable", "The model `"+route.Slug+"` has an input content policy that cannot be enforced on the `"+string(wire)+"` provider wire.")
}
