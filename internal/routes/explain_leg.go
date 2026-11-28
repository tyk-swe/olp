package routes

import (
	"cmp"
	"slices"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/contracts/interaction"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operationregistry"
	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

func resolveSimulationAttribution(input simulationInput, key *inspectionKeyContext) (simulationInput, error) {
	if key.id != "" {
		for name := range input.Attribution {
			if !slices.Contains(key.authority.Policy.AllowedAttributionKeys, name) && key.authority.Policy.AttributionDefaults[name] == "" && key.authority.ProjectAttributionPolicy.Defaults[name] == "" {
				return simulationInput{}, problem.Invalid("attribution", "An attribution label is not allowed by the selected key.")
			}
		}
	}
	labels, err := key.authority.ResolveAttribution(input.Attribution)
	if err != nil {
		return simulationInput{}, problem.Invalid("attribution", "Attribution does not satisfy the key and project policies.")
	}
	key.authority.Attribution, input.Attribution = labels, labels
	return input, nil
}

func buildLegPrepare(m *simulation, named, route routing.Route, key inspectionKeyContext, eligibility func(string) routing.Eligibility, input simulationInput, semantic interaction.Context, inspectionRequest *wire.Request, unary *oif.Request, mediaRequest *media.Request, counted *simulatedDemand, workflow bool, workflowCapability *routing.WorkflowCapability) (routing.PrepareCandidate, map[string]*interactionInspection) {
	inspected, inspections, effectiveRequests := inspectionAccept(route, inspectionRequest, semantic, counted, workflow)
	prepare := workflowInspectionAccept(workflow, inspected, effectiveRequests, m, workflowCapability)
	if unary != nil {
		prepare, inspections = inspectionUnaryAccept(route, cmp.Or(m.named, route.Slug), *unary, semantic, input.ClientContract, m.demand)
	}
	if !operationregistry.PolicyFor(input.Operation).BoundedWork {
		prior := prepare
		prepare = func(provider routing.Provider, target routing.Target) (*routing.CandidatePreparation, error) {
			costExempt := named.CallerCostExempt && provider.CredentialSource == "caller"
			budgeted := key.authority.UnboundedWorkBudgeted(named.Slug, costExempt) || routing.UnboundedWorkBudgeted(nil, nil, &named, costExempt) || routing.UnboundedWorkBudgeted(&provider, nil, &route, costExempt)
			for _, slot := range routing.SelectSlots(provider, target.ProviderModel, route, key.id, input.Operation, input.Surface, input.Mode, []byte(input.Seed)) {
				if eligibility != nil && slot.CredentialID != nil && eligibility(*slot.CredentialID) != routing.Eligible {
					continue
				}
				budgeted = budgeted || routing.UnboundedWorkBudgeted(&provider, &slot, &route, costExempt)
			}
			if budgeted {
				inspections[target.ID] = &interactionInspection{Status: "incompatible", Fidelity: route.Fidelity.Mode, Evidence: []string{}}
				return nil, &inspectionDiagnostic{"unbounded_work_budget", "/limits", "bounded_provider_work", "Decisions cannot enforce token or cost budgets."}
			}
			if prior != nil {
				return prior(provider, target)
			}
			return nil, nil
		}
	}
	if mediaRequest != nil {
		prepare, inspections = inspectionMediaAccept(route, mediaRequest, input.Dialect, semantic, input.ClientContract, m.demand)
	}
	return prepare, inspections
}

func (s *Server) buildLegOptions(m *simulation, named, route routing.Route, key inspectionKeyContext, eligibility func(string) routing.Eligibility, input simulationInput, via usage.LegVia, parsed *wire.Request, mediaRequest *media.Request, counted *simulatedDemand, prepare routing.PrepareCandidate) routing.Options {
	preferences := input.Preferences
	if via != "" && preferences != nil {
		// Like the gateway, a route the request moves to keeps the caller's
		// narrowing but not its attempt count, which the named route spent.
		copied := *preferences
		copied.MaxAttempts = nil
		preferences = &copied
	}
	now := m.now
	options := routing.Options{
		Region:  s.Region,
		Context: m.ctx, KeyID: key.id, Preferences: preferences, Inputs: m.inputs, TokenDemand: counted.fixed, Demand: counted.sourceDemand(),
		CheckSlots: true, CredentialEligibility: eligibility, UnconfinedPlugins: s.UnconfinedPlugins,
		Prepare: prepare, Supply: m.supply, Unhealthy: m.unhealthy, Now: now,
		// A simulation reports what each target could cost and applies the
		// caller's cost ceiling to the same worst case.
		EstimateCost: true,
		WorstCost: func(provider routing.Provider, target routing.Target, price *rates.RoutingPrice, discount *rates.RoutingDiscount) (routing.CostBound, bool) {
			return simulatedWorstCost(input.Operation, mediaRequest, counted, provider, target, price, discount, now)
		},
		CallerCostExempt: named.CallerCostExempt,
	}
	if key.reason != "" {
		options.Prepare = nil
	}
	if parsed != nil {
		options.Parameters = sync.OnceValue(func() []string { return protocols.ParameterNames(parsed) })
	}
	if len(route.Selectors) > 0 {
		options.Features = routing.DescribeRequest(input.Operation, parsed, counted.request())
		options.Evaluate, options.Permitted = m.evaluate, m.permitted
	}
	if route.Shadowed() {
		options.Mirror = []byte(input.Seed)
	}
	return options
}

func simulatedWorstCost(operation string, mediaRequest *media.Request, counted *simulatedDemand, provider routing.Provider, target routing.Target, price *rates.RoutingPrice, discount *rates.RoutingDiscount, now time.Time) (routing.CostBound, bool) {
	if !operationregistry.PolicyFor(operation).BoundedWork || price == nil {
		return routing.CostBound{}, false
	}
	if provider.CommitmentAt(now) != nil {
		return routing.CostBound{}, true
	}
	if mediaRequest != nil {
		seconds, video := mediaRequest.VideoSeconds()
		if !video {
			return routing.CostBound{}, false
		}
		cost, ok := price.Cost(usage.AttemptUsage{Evidence: usage.EvidenceUnobservedComplete, UsageQuantities: usage.UsageQuantities{MediaUnits: &seconds}}, now, true, discount)
		return routing.CostBound{Cost: cost}, ok
	}
	if operation == "search" {
		cost, ok := price.Cost(usage.AttemptUsage{Evidence: usage.EvidenceUnobservedComplete}, now, true, discount)
		return routing.CostBound{Cost: cost}, ok
	}
	if counted.prompt != nil {
		from := counted.prompt.Estimate(catalog.TokenCounter(target.ProviderModel), nil)
		prepared, ok := counted.estimates[target.ID]
		to := prepared.Estimate
		if !ok {
			to = from
		}
		if counted.output != nil {
			from.Output, to.Output = counted.output, counted.output
		}
		if counted.prompt.Request().Family == wire.Completions && from.Output == nil && to.Output != nil {
			from.Output = to.Output
		}
		input, output := max(from.InputExposure(), to.InputExposure()), max(from.Reply(), to.Reply())
		cost, priced := price.CostBound(input, output, output > 0, now, discount)
		return routing.CostBound{Cost: cost, InputTokens: &input, OutputTokens: &output}, priced
	}
	d := counted.fixed
	if demand := counted.sourceDemand(); demand != nil {
		d = demand(provider, target)
	}
	if d == nil {
		return routing.CostBound{}, false
	}
	output := int64(0)
	if d.MaxOutputTokens != nil {
		output = *d.MaxOutputTokens
	} else if operation == "generation" {
		output = estimate.DefaultOutputTokens
	}
	input := d.EstimatedInputTokens
	cost, ok := price.CostBound(input, output, output > 0, now, discount)
	return routing.CostBound{Cost: cost, InputTokens: &input, OutputTokens: &output}, ok
}
