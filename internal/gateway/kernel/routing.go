package kernel

import (
	"context"
	"errors"
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

// selectionOptions supplies the shared routing authority and measurements.
// Each operation adds its own source demand and semantic preparation.
func (s *Server) selectionOptions(ctx context.Context, x *Call) routing.Options {
	inputs := s.Deps.Runtime.RoutingInputs()
	x.routingInputs = inputs
	options := routing.Options{
		Region: s.Config.Region,
		KeyID:  x.keyID, Preferences: x.legPreferences(),
		Inputs: inputs, Now: s.Now(),
		CheckSlots: true, CredentialEligibility: s.Deps.Runtime.Eligibility,
		UnconfinedPlugins: s.Config.UnconfinedPlugins, Context: ctx,
		CallerCostExempt: x.callerCostExempt(),
	}
	if s.Deps.Admission.Ready() {
		options.Supply = s.Deps.Admission
	}
	if s.fleet.Marked() {
		options.Unhealthy = s.unhealthy
	}
	options.Mirror = x.mirrorSeed()
	options.WorstCost = func(provider routing.Provider, target routing.Target, price *rates.RoutingPrice, discount *rates.RoutingDiscount) (routing.CostBound, bool) {
		return x.targetCostBound(provider, target, price, discount)
	}
	// Only a route with selectors pays to describe the request to them.
	if x.Route != nil && len(x.Route.Selectors) > 0 {
		options.Features = x.features()
		options.Evaluate = s.evaluator(ctx, x, options.Features)
		if x.authorize != nil || x.fixed {
			options.Permitted = func(route routing.Route) bool {
				return !x.fixed && (x.authorize == nil || x.authorize(&route) == nil)
			}
		}
	}
	return options
}

// features describe the caller's request to route selectors, from what
// admission already computed: its estimated size, whether it streams, and the
// tools, modalities, structured output and reasoning effort it asks for.
// Attribution labels never take part.
func (x *Call) features() *routing.Features {
	if x.generation.parsed == nil {
		return routing.DescribeRequest(x.OperationName(), nil, nil)
	}
	return routing.DescribeRequest(x.OperationName(), x.generation.parsed, x.sourceDemand(""))
}

type planningKind uint8

const (
	canonicalPlanning planningKind = iota
	unaryPlanning
	surfacePlanning
	pinnedPlanning
)

type SurfacePlanRequest struct {
	Operation  string
	Surface    string
	Mode       string
	Parameters []string
	Eligible   func(routing.Provider, routing.Target) error
	Prepare    func(routing.Provider, routing.Target) (*routing.CandidatePreparation, error)
}

func (s *Server) SelectSurfacePlan(ctx context.Context, x *Call, req SurfacePlanRequest) *fault.Error {
	session := s.planning(x, surfacePlanning, x.Route, req.Operation, req.Surface, req.Mode)
	options := s.selectionOptions(ctx, x)
	options.Parameters = routing.Listed(req.Parameters)
	if req.Eligible != nil || req.Prepare != nil {
		options.Prepare = func(p routing.Provider, t routing.Target) (*routing.CandidatePreparation, error) {
			if req.Eligible != nil {
				if err := req.Eligible(p, t); err != nil {
					return nil, err
				}
			}
			if req.Prepare == nil {
				return &routing.CandidatePreparation{Parameters: options.Parameters}, nil
			}
			prep, err := req.Prepare(p, t)
			if err != nil {
				session.semantic = err
				return nil, err
			}
			return prep, nil
		}
	}
	_, _, failure := session.selectPlan(ctx, options)
	return failure
}

type planningSession struct {
	server    *Server
	call      *Call
	route     *routing.Route
	kind      planningKind
	operation string
	surface   string
	mode      string
	semantic  error
	unbounded bool
}

func (s *Server) planning(x *Call, kind planningKind, route *routing.Route, operation, surface, mode string) *planningSession {
	return &planningSession{server: s, call: x, kind: kind, route: route, operation: operation, surface: surface, mode: mode}
}

func (p *planningSession) selectPlan(ctx context.Context, options routing.Options) (routing.Plan, bool, *fault.Error) {
	x := p.call
	plan, err := routing.Select(x.Snapshot(), p.route.Slug, p.operation, p.surface, p.mode, x.affinity, options)
	if err != nil {
		if p.kind == canonicalPlanning || p.kind == surfacePlanning {
			var selection *routing.SelectionError
			if errors.As(err, &selection) && selection.Code != routing.NoEligibleTargets && selection.Code != "attempt_budget_increase_forbidden" {
				return plan, false, selectionError(err, p.route.Slug)
			}
		}
		return plan, false, RequestError(err)
	}
	if plan.Delegate != "" && p.kind != pinnedPlanning {
		return plan, true, p.server.delegate(ctx, x, plan)
	}
	if p.kind != pinnedPlanning {
		x.adoptPlan(plan)
	}
	if len(plan.Attempts) != 0 {
		if p.kind == canonicalPlanning && x.pin != nil {
			x.budget, x.allowance = x.spent+1, x.spent+1
		}
		return plan, false, nil
	}
	if p.semantic != nil {
		problem := RequestError(p.semantic)
		if p.unbounded && problem.Code == "target_capability" {
			problem.Status = http.StatusUnprocessableEntity
		}
		return plan, false, problem
	}
	if p.kind == canonicalPlanning && x.Strict() {
		for _, decision := range plan.Decisions {
			if decision.Reason != nil && (*decision.Reason == "max_output_tokens_exceeded" || *decision.Reason == "context_length_exceeded") {
				return plan, false, fault.InvalidRequest(*decision.Reason, "The effective invocation exceeds a declared model limit; its controls were not reduced.", nil)
			}
		}
	}
	x.noteCostExceeded(plan)
	if x.costExcluded {
		return plan, false, costLimitExceeded()
	}
	return plan, false, selectionError(&routing.SelectionError{Code: routing.NoEligibleTargets}, p.route.Slug)
}

func (p *planningSession) adoptPin(plan routing.Plan) {
	p.call.decisions, p.call.policy = plan.Decisions, plan.Policy
	p.call.budget = 1
	p.call.attempts = plan.Attempts[:1]
}
