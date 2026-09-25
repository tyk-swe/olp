package gateway

import (
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/runtime"
)

type preparedProvider struct {
	invocation      providerinvoke.Invocation
	estimate        int64
	parameters      []string
	demand          *runtime.TokenDemand
	plan            *interaction.Plan
	policyDecisions []contentpolicy.Decision
}

// A summary belongs to one source or one bound destination within an inference
// request. The same complete token walk feeds routing and reservation; the
// canonical parameter names still come from the effective request adapter.
type requestSummary struct {
	request    *openai.Request
	parameters []string
	demand     *runtime.TokenDemand
	estimate   int64
}

func summarizeRequest(request *openai.Request) requestSummary {
	input, output, candidates := estimateParts(request)
	return requestSummary{
		request:    request,
		parameters: protocols.ParameterNames(request),
		demand:     &runtime.TokenDemand{EstimatedInputTokens: input, MaxOutputTokens: output},
		estimate:   estimateTokensFromParts(request, input, output, candidates),
	}
}

func (x *execution) summarizeSource() requestSummary {
	if x.parsed == nil {
		// A registration-only dialect has no legacy request view; the
		// registered source dialect still owns its reservation shape.
		if x.sourceSummary == nil && x.gen != nil {
			est := x.gen.Estimate(x.source.Request.Document())
			x.sourceSummary = &requestSummary{parameters: x.gen.Parameters(x.source.Request.Document()), demand: &runtime.TokenDemand{EstimatedInputTokens: est.Input, MaxOutputTokens: est.Output}, estimate: est.Total()}
		}
		if x.sourceSummary == nil {
			return requestSummary{}
		}
		return *x.sourceSummary
	}
	if x.sourceSummary == nil || x.sourceSummary.request != x.parsed {
		summary := summarizeRequest(x.parsed)
		x.sourceSummary = &summary
	}
	return *x.sourceSummary
}

func (x *execution) strict() bool {
	return x.route != nil && runtime.FidelityMode(x.route.Fidelity) == runtime.FidelityStrict
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
		plan, err := template.Bind(x.source, binding)
		if err != nil {
			return preparedProvider{}, err
		}
		decisions, err := plan.CheckInput()
		source := x.summarizeSource()
		estimate := plan.Estimate()
		effective := requestSummary{parameters: plan.Parameters(), demand: &runtime.TokenDemand{EstimatedInputTokens: estimate.Input, MaxOutputTokens: estimate.Output}, estimate: estimate.Total()}
		prepared := preparedProvider{plan: plan, invocation: providerinvoke.Invocation{Prepared: plan.Prepared(), Wire: plan.Wire()}, estimate: max(source.estimate, effective.estimate), parameters: effective.parameters, demand: effective.demand, policyDecisions: decisions}
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
	if compiled != nil {
		for _, rule := range compiled.Input {
			matched, blocked := false, false
			next := protocols.InspectInputText(native, func(text string) (string, bool) {
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
	source, effective := x.summarizeSource(), summarizeRequest(native)
	estimate := max(source.estimate, effective.estimate)
	// Bedrock's native tool catalogue is outside the OpenAI tools field. Its
	// whole schema still contributes to the same conservative token reservation.
	if invocation.Wire == openai.FamilyBedrock {
		estimate = addBounded(estimate, estimateSchema(native.Field("toolConfig")))
	}
	prepared := preparedProvider{invocation: invocation, estimate: estimate, parameters: effective.parameters, demand: effective.demand, policyDecisions: decisions}
	if x.preparedProviders == nil {
		x.preparedProviders = map[string]preparedProvider{}
	}
	x.preparedProviders[key] = prepared
	return prepared, nil
}

func (x *execution) providerEstimate(provider *runtime.Provider) int64 {
	if x.unary != nil {
		var estimate int64
		for _, a := range x.attempts {
			if a.ProviderID == provider.ID {
				plan, err := x.unaryPlan(provider, a.UpstreamModel)
				if err != nil {
					return limits.MaxCounter
				}
				estimate = max(estimate, plan.Estimate())
			}
		}
		return max(estimate, 1)
	}
	if provider.ProfileID == "" && !x.strict() && (x.route == nil || x.route.ContentPolicy == nil) {
		return estimateTokens(x.parsed, provider.ParameterDefaults)
	}
	var estimate int64
	for _, attempt := range x.attempts {
		if attempt.ProviderID != provider.ID {
			continue
		}
		prepared, err := x.preparedProvider(provider, attempt.UpstreamModel)
		if err != nil {
			return limits.MaxCounter
		}
		estimate = max(estimate, prepared.estimate)
	}
	return max(estimate, 1)
}
