package routes

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
)

// fallbackStandby marks a fallback that only a failed attempt could start, so
// simulation, which dispatches nothing, can name it but not take it.
const fallbackStandby = "standby"

// simulation evaluates one sample request the way the gateway plans it: the
// named route first, then the route a selector delegates to, and then the
// fallbacks that conditions known before dispatch, such as an exceeded context
// window or an exhausted spend cap, start. It reads the capacity, spend-cap and
// shared circuit state the gateways read, but never reserves state or contacts
// a provider: classifier predicates use the labels the caller supplies.
type simulation struct {
	ctx              context.Context
	snapshot         *routing.Snapshot
	input            simulationInput
	inputs           *usage.RoutingInputs
	demand           *routing.TokenDemand
	inspectionBudget inspectionBudget
	// named is the route the request names; every leg parses the request as
	// addressed to it, as the gateway parses it once.
	named     string
	supply    routing.SupplyReader
	unhealthy func(providerID string) bool
	// key reads the selected API key's standing on a route, and eligibility
	// the credential eligibility of a route's targets.
	key         func(routing.Route) (inspectionKeyContext, error)
	eligibility func(routing.Route) (func(string) routing.Eligibility, error)
}

// simulatedLeg is one route the request plans on.
type simulatedLeg struct {
	Route            string                      `json:"route"`
	Via              *string                     `json:"via"`
	Selectors        []routing.SelectorOutcome   `json:"selectors"`
	Affinity         *simulatedAffinity          `json:"affinity"`
	Decisions        []inspectedDecision         `json:"decisions"`
	GatewayExecution *routing.WorkflowCapability `json:"gateway_execution,omitempty"`
}

// simulatedAffinity says whether the request carried the session key the route
// keeps sessions by; when it does, that key seeds the target and slot ranking
// in place of the simulation seed.
type simulatedAffinity struct {
	Source  string  `json:"source"`
	Label   *string `json:"label"`
	Session bool    `json:"session"`
}

// explanation is a simulated request across routes: every leg it plans on, the
// named route first, and every fallback the gateway would consider.
type explanation struct {
	Legs      []simulatedLeg         `json:"legs"`
	Fallbacks []routing.FallbackStep `json:"fallbacks"`
}

// newSimulation reads the shared state a simulation plans with once.
func (s *Server) newSimulation(ctx context.Context, snapshot *routing.Snapshot, input simulationInput, inputs *usage.RoutingInputs, demand *routing.TokenDemand) *simulation {
	m := &simulation{ctx: ctx, snapshot: snapshot, input: input, inputs: inputs, demand: demand, supply: s.Supply}
	if s.Fleet != nil {
		if circuits, err := s.Fleet.Circuits(ctx, time.Now()); err == nil && len(circuits) > 0 {
			m.unhealthy = func(providerID string) bool { _, open := circuits[providerID]; return open }
		}
	}
	return m
}

// permitted reports whether the selected key may use a route; without a
// selected key every route is.
func (m *simulation) permitted(route routing.Route) bool {
	key, err := m.key(route)
	return err == nil && key.reason == ""
}

// evaluate answers a selector's classifier from the labels the caller supplied.
// Plugin predicates are not simulated.
func (m *simulation) evaluate(selector routing.Selector) routing.PredicateResult {
	classifier := selector.When.Classifier
	if classifier == nil {
		return routing.PredicateResult{Failure: "plugin_not_simulated"}
	}
	label, ok := m.input.ClassifierLabels[selector.ID]
	if !ok {
		return routing.PredicateResult{Failure: "classifier_not_simulated"}
	}
	return routing.PredicateResult{Matched: classifier.Accepts(label, nil), Label: label}
}

// leg plans the request on one route. The plan serves only when the key may
// use the route and it ranks at least one attempt.
func (s *Server) leg(m *simulation, slug, via string) (simulatedLeg, routing.Plan, bool, error) {
	route := m.snapshot.Routes[slug]
	key, err := m.key(route)
	if err != nil {
		return simulatedLeg{}, routing.Plan{}, false, err
	}
	eligibility, err := m.eligibility(route)
	if err != nil {
		return simulatedLeg{}, routing.Plan{}, false, err
	}
	input := m.input
	if key.id != "" {
		for name := range input.Attribution {
			if !slices.Contains(key.authority.Policy.AllowedAttributionKeys, name) && key.authority.Policy.AttributionDefaults[name] == "" && key.authority.ProjectAttributionPolicy.Defaults[name] == "" {
				return simulatedLeg{}, routing.Plan{}, false, access.Invalid("attribution", "An attribution label is not allowed by the selected key.")
			}
		}
	}
	labels, err := key.authority.ResolveAttribution(input.Attribution)
	if err != nil {
		return simulatedLeg{}, routing.Plan{}, false, access.Invalid("attribution", "Attribution does not satisfy the key and project policies.")
	}
	key.authority.Attribution, input.Attribution = labels, labels
	semantic, err := inspectionContext(input.SemanticHeaders, input.QuerySettings, key.allowProviderState)
	if err != nil {
		return simulatedLeg{}, routing.Plan{}, false, err
	}
	parsed, unary, mediaRequest, err := inspectorAnyRequest(input.Request, input.Operation, input.Surface, input.Mode, input.Dialect, cmp.Or(m.named, slug), route.Fidelity.Strict())
	if err != nil {
		return simulatedLeg{}, routing.Plan{}, false, err
	}
	named := m.snapshot.Routes[cmp.Or(m.named, slug)]
	workflowReason := s.workflowInspectionReason(m, named, route, key, parsed)
	workflowCapability := s.workflowInspectionCapability(m, named, route, key, parsed)
	if input.Operation == "generation" {
		if err = inspectionClientContract(input.ClientContract, &semantic, s.Access.Keys != nil); err != nil {
			return simulatedLeg{}, routing.Plan{}, false, err
		}
	}
	workflow := input.Operation == "generation" && named.ToolExecution == "gateway"
	inspectionRequest, err := workflowInspectionRequest(parsed, workflow)
	if err != nil {
		return simulatedLeg{}, routing.Plan{}, false, access.Invalid("request", "The workflow controls could not be processed.")
	}
	counted := newSimulatedDemand(parsed, input, m.demand)
	inspected, effective, inspections, effectiveRequests := inspectionAccept(route, inspectionRequest, semantic, counted, &m.inspectionBudget, workflow)
	accept := workflowInspectionAccept(workflow, inspected, effectiveRequests, m, workflowCapability)
	if unary != nil {
		accept, effective, inspections = inspectionUnaryAccept(route, cmp.Or(m.named, slug), *unary, semantic, input.ClientContract, m.demand, &m.inspectionBudget)
	}
	if operationregistry.UnboundedWork(input.Operation) {
		prepare := accept
		named := m.snapshot.Routes[cmp.Or(m.named, slug)]
		accept = func(provider routing.Provider, target routing.Target) error {
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
				return &inspectionDiagnostic{"unbounded_work_budget", "/limits", "bounded_provider_work", "Decisions cannot enforce token or cost budgets."}
			}
			if prepare != nil {
				return prepare(provider, target)
			}
			return nil
		}
	}
	if mediaRequest != nil {
		accept, effective, inspections = inspectionMediaAccept(route, mediaRequest, input.Dialect, semantic, input.ClientContract, m.demand, &m.inspectionBudget)
	}
	preferences := input.Preferences
	if via != "" && preferences != nil {
		// Like the gateway, a route the request moves to keeps the caller's
		// narrowing but not its attempt count, which the named route spent.
		copied := *preferences
		copied.MaxAttempts = nil
		preferences = &copied
	}
	now := time.Now()
	options := routing.Options{
		Region:  s.Region,
		Context: m.ctx, KeyID: key.id, Preferences: preferences, Inputs: m.inputs, TokenDemand: counted.fixed, Demand: counted.sourceDemand(),
		CheckSlots: true, CredentialEligibility: eligibility, UnconfinedPlugins: s.UnconfinedPlugins,
		Accept: accept, Effective: effective, Supply: m.supply, Unhealthy: m.unhealthy, Now: now,
		// A simulation reports what each target could cost and applies the
		// caller's cost ceiling to the same worst case.
		EstimateCost: true,
		WorstCost: func(provider routing.Provider, target routing.Target, price *usage.RoutingPrice, discount *usage.RoutingDiscount) (usage.Cost, bool) {
			if operationregistry.UnboundedWork(input.Operation) {
				return usage.Cost{}, false
			}
			if provider.Commitment != nil {
				return usage.Cost{}, true
			}
			if mediaRequest != nil {
				if mediaRequest.Op != media.OpVideoCreate {
					return usage.Cost{}, false
				}
				seconds := "4"
				if mediaRequest.Seconds != nil {
					seconds = *mediaRequest.Seconds
				}
				return price.Cost(usage.AttemptUsage{Complete: true, MediaUnits: &seconds}, now, true, discount)
			}
			if input.Operation == "search" {
				return price.Cost(usage.AttemptUsage{Complete: true}, now, true, discount)
			}
			if counted.prompt != nil {
				from := counted.prompt.Estimate(estimate.ForModel(target.ProviderModel), nil)
				to, ok := counted.estimates[target.ID]
				if !ok {
					to = from
				}
				if counted.output != nil {
					from.Output, to.Output = counted.output, counted.output
				}
				if counted.prompt.Request().Family == wire.Completions && from.Output == nil && to.Output != nil {
					from.Output = to.Output
				}
				output := max(from.Reply(), to.Reply())
				return price.CostBound(max(from.InputExposure(), to.InputExposure()), output, output > 0, now, discount)
			}
			d := counted.fixed
			if demand := counted.sourceDemand(); demand != nil {
				d = demand(provider, target)
			}
			if d == nil {
				return usage.Cost{}, false
			}
			output := int64(0)
			if d.MaxOutputTokens != nil {
				output = *d.MaxOutputTokens
			} else if input.Operation == "generation" {
				output = estimate.DefaultOutputTokens
			}
			return price.CostBound(d.EstimatedInputTokens, output, output > 0, now, discount)
		},
	}
	if key.reason != "" {
		options.Accept = nil
		options.Effective = nil
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
	plan, err := routing.Select(m.snapshot, slug, input.Operation, input.Surface, input.Mode, route.Affinity.Seed(input.Attribution, parsed, []byte(input.Seed)), options)
	if err != nil {
		return simulatedLeg{}, routing.Plan{}, false, err
	}
	applyInspectionKeyReason(plan.Decisions, key.reason)
	applyInspectionKeyReason(plan.Decisions, workflowReason)
	leg := simulatedLeg{
		Route: slug, Selectors: plan.Selectors,
		GatewayExecution: workflowCapability,
		Decisions:        inspectedDecisions(plan.Decisions, route, parsed != nil || unary != nil || mediaRequest != nil, inspections, counted.estimates, workflowCapability),
	}
	if leg.Selectors == nil {
		leg.Selectors = []routing.SelectorOutcome{}
	}
	if via != "" {
		leg.Via = &via
	}
	if affinity := route.Affinity; affinity != nil {
		leg.Affinity = &simulatedAffinity{Source: affinity.Source, Session: affinity.SessionKey(input.Attribution, parsed) != ""}
		if affinity.Label != "" {
			leg.Affinity.Label = &affinity.Label
		}
	}
	return leg, plan, key.reason == "" && workflowReason == "", nil
}

func workflowInspectionRequest(request *wire.Request, workflow bool) (*wire.Request, error) {
	if !workflow || request == nil {
		return request, nil
	}
	toolsets := oif.Change{Pointer: oif.Pointer("", "olp_toolsets"), Remove: true, Reason: "gateway toolset selection"}
	limits := oif.Change{Pointer: oif.Pointer("", "olp_run_limits"), Remove: true, Reason: "gateway workflow bounds"}
	switch {
	case len(request.Field("olp_toolsets")) > 0 && len(request.Field("olp_run_limits")) > 0:
		return request.WithResourceBindings(toolsets, limits)
	case len(request.Field("olp_toolsets")) > 0:
		return request.WithResourceBindings(toolsets)
	case len(request.Field("olp_run_limits")) > 0:
		return request.WithResourceBindings(limits)
	default:
		return request, nil
	}
}

func workflowInspectionAccept(workflow bool, prior func(routing.Provider, routing.Target) error, requests map[string]*wire.Request, m *simulation, capability *routing.WorkflowCapability) func(routing.Provider, routing.Target) error {
	if !workflow {
		return prior
	}
	return func(provider routing.Provider, target routing.Target) error {
		if !provider.SupportsToolCalls(target.ProviderModel, m.input.Surface, m.input.Mode) {
			return &inspectionDiagnostic{"tool_execution_unsupported", "/tools", "certified_tool_calls", "The target is not certified for gateway tool execution."}
		}
		if prior != nil {
			if err := prior(provider, target); err != nil {
				return err
			}
		}
		return workflowModelInspectionCost(m, provider, target, requests[target.ID], capability)
	}
}

func (s *Server) workflowInspectionReason(m *simulation, route, serving routing.Route, key inspectionKeyContext, request *wire.Request) string {
	if m.input.Operation != "generation" || route.ToolExecution != "gateway" || key.reason != "" {
		return ""
	}
	if route.Fidelity.Strict() || serving.Fidelity.Strict() {
		return "tool_execution_requires_transformed"
	}
	if key.id == "" || s.WorkflowTools == nil || s.WorkflowTools() == nil || s.WorkflowRelease == nil || s.WorkflowAvailable == nil || !s.WorkflowAvailable() {
		return "tool_execution_unavailable"
	}
	if refusal := mcp.WorkflowRequestRefusal(request); refusal != "" {
		return refusal
	}
	if _, valid := workflowCallerBounds(request); !valid {
		return "invalid_run_limits"
	}
	var raw []byte
	if request != nil {
		raw = request.Field("olp_toolsets")
	}
	selected, err := mcp.SelectWorkflowToolsets(route.Toolsets, raw)
	if err != nil {
		return "toolset_not_permitted"
	}
	if _, err := s.WorkflowTools().Resolve(m.ctx, key.authority, s.WorkflowRelease(), selected); err != nil {
		return "toolset_not_authorized"
	}
	return ""
}

func workflowCallerBounds(request *wire.Request) (*limits.WorkflowBounds, bool) {
	if request == nil || len(request.Field("olp_run_limits")) == 0 {
		return nil, true
	}
	bounds, valid := limits.ParseWorkflowBounds(request.Field("olp_run_limits"))
	return &bounds, valid
}

func (s *Server) workflowInspectionCapability(m *simulation, named, serving routing.Route, key inspectionKeyContext, request *wire.Request) *routing.WorkflowCapability {
	if m.input.Operation != "generation" || s.WorkflowTools == nil || s.WorkflowRelease == nil || s.WorkflowAvailable == nil {
		return nil
	}
	caller, valid := workflowCallerBounds(request)
	if !valid {
		return nil
	}
	return mcp.DescribeWorkflow(m.ctx, s.WorkflowTools(), key.authority, s.WorkflowRelease(), named, serving, m.input.Surface, caller, s.WorkflowAvailable(), time.Now())
}

func workflowModelInspectionCost(m *simulation, provider routing.Provider, target routing.Target, request *wire.Request, capability *routing.WorkflowCapability) error {
	if capability == nil || capability.Limits.MaxRunCost == nil {
		return nil
	}
	limit, ok := usage.ParseCost(*capability.Limits.MaxRunCost)
	if !ok || request == nil {
		return workflowUnboundedInspection()
	}
	var metadata routing.ModelMetadata
	if err := json.Unmarshal(provider.Models[target.ProviderModel], &metadata); err != nil || metadata.ContextLength == nil || *metadata.ContextLength < 1 {
		return workflowUnboundedInspection()
	}
	output, _ := estimate.OutputBounds(request, nil)
	if output == nil || *output < 1 {
		return workflowUnboundedInspection()
	}
	now := time.Now()
	price := m.inputs.Price(provider.Kind, provider.ID, provider.VendorID, target.ProviderModel, "generation", now)
	discount := m.inputs.Discount(provider.Kind, provider.ID, provider.VendorID, now)
	bound, priced := price.WorkflowModelCostBound(*metadata.ContextLength, *output, discount)
	if !priced {
		return workflowUnboundedInspection()
	}
	if bound.Cmp(limit) > 0 {
		return &inspectionDiagnostic{"run_cost_limit", "/olp_run_limits/max_run_cost", "remaining_run_cost", "The model cost exceeds the run limit."}
	}
	return nil
}

func workflowUnboundedInspection() error {
	return &inspectionDiagnostic{"run_cost_unbounded", "/olp_run_limits/max_run_cost", "bounded_model_cost", "The model cost cannot be bounded."}
}

// explain simulates the request from the named route on, following the same
// delegation and fallback walk as the gateway.
func (s *Server) explain(m *simulation, slug string) (explanation, error) {
	m.named = slug
	first, plan, permitted, err := s.leg(m, slug, "")
	if err != nil {
		return explanation{}, err
	}
	route := m.snapshot.Routes[slug]
	w := &walk{s: s, m: m, route: &route, visited: []string{slug}}
	w.out = explanation{Legs: []simulatedLeg{first}, Fallbacks: []routing.FallbackStep{}}
	if !permitted {
		return w.out, nil
	}
	if w.settle(plan) == routing.FallbackPlanned || w.fallBack() {
		w.standby()
	}
	return w.out, nil
}

// walk is the route state of a simulated request as it moves between routes.
type walk struct {
	s          *Server
	m          *simulation
	route      *routing.Route
	delegators []*routing.Route
	visited    []string
	conditions []string
	frames     []walkFrame
	out        explanation
}

type walkFrame struct {
	route      *routing.Route
	conditions []string
	next       int
}

// settle follows a plan's selector delegation and records the conditions an
// unserved plan names.
func (w *walk) settle(plan routing.Plan) string {
	w.conditions = nil
	if plan.Delegate != "" {
		if w.adopt(plan.Delegate, usage.ViaSelector) != routing.FallbackPlanned {
			return routing.NoEligibleTargets
		}
		return routing.FallbackPlanned
	}
	if len(plan.Attempts) == 0 {
		w.conditions = plan.Conditions()
		return routing.NoEligibleTargets
	}
	return routing.FallbackPlanned
}

// adopt moves the request to another route, as the gateway does.
func (w *walk) adopt(slug, via string) string {
	if slices.Contains(w.visited, slug) {
		return routing.FallbackRepeated
	}
	route, ok := w.m.snapshot.Routes[slug]
	if !ok {
		return routing.FallbackUnavailable
	}
	if !w.m.permitted(route) {
		return routing.FallbackForbidden
	}
	if via == usage.ViaSelector {
		w.delegators = append(w.delegators, w.route)
	} else {
		w.delegators = nil
	}
	w.route, w.visited = &route, append(w.visited, slug)
	leg, plan, permitted, err := w.s.leg(w.m, slug, via)
	if err != nil {
		w.conditions = nil
		var refusal *routing.SelectionError
		if errors.As(err, &refusal) {
			return refusal.Code
		}
		return routing.NoEligibleTargets
	}
	w.out.Legs = append(w.out.Legs, leg)
	if !permitted {
		w.conditions = nil
		return routing.NoEligibleTargets
	}
	return w.settle(plan)
}

// push frames the serving route and the routes that delegated to it, as the
// gateway does, so the serving route's fallbacks come first.
func (w *walk) push() {
	for _, route := range append(slices.Clone(w.delegators), w.route) {
		if len(route.Fallbacks) > 0 && !slices.ContainsFunc(w.frames, func(f walkFrame) bool { return f.route.Slug == route.Slug }) {
			w.frames = append(w.frames, walkFrame{route: route, conditions: w.conditions})
		}
	}
}

// fallBack walks the fallbacks the plan-time conditions start, depth first, and
// reports whether one of them plans attempts.
func (w *walk) fallBack() bool {
	if len(w.conditions) == 0 {
		return false
	}
	w.push()
	for len(w.frames) > 0 {
		top := &w.frames[len(w.frames)-1]
		if top.next == len(top.route.Fallbacks) {
			w.frames = w.frames[:len(w.frames)-1]
			continue
		}
		fallback := top.route.Fallbacks[top.next]
		from := top.route.Slug
		top.next++
		met := fallback.Met(top.conditions)
		if len(met) == 0 {
			continue
		}
		step := routing.FallbackStep{From: from, Route: fallback.Route, Conditions: met}
		step.Outcome = w.adopt(fallback.Route, usage.ViaFallback)
		w.out.Fallbacks = append(w.out.Fallbacks, step)
		if step.Outcome == routing.FallbackPlanned {
			return true
		}
		if len(w.conditions) > 0 {
			w.push()
		}
	}
	return false
}

// standby names, in the order the gateway would consider them, the fallbacks
// that only a failed attempt could still start.
func (w *walk) standby() {
	w.conditions = nil
	w.push()
	for i := len(w.frames) - 1; i >= 0; i-- {
		frame := w.frames[i]
		for _, fallback := range frame.route.Fallbacks[frame.next:] {
			step := routing.FallbackStep{From: frame.route.Slug, Route: fallback.Route, Conditions: fallback.On, Outcome: fallbackStandby}
			target, ok := w.m.snapshot.Routes[fallback.Route]
			switch {
			case slices.Contains(w.visited, fallback.Route):
				step.Outcome = routing.FallbackRepeated
			case !ok:
				step.Outcome = routing.FallbackUnavailable
			case !w.m.permitted(target):
				step.Outcome = routing.FallbackForbidden
			}
			w.out.Fallbacks = append(w.out.Fallbacks, step)
		}
	}
}
