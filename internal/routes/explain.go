package routes

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/runtime"
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
	snapshot         *runtime.Snapshot
	input            simulationInput
	inputs           *usage.RoutingInputs
	demand           *runtime.TokenDemand
	inspectionBudget inspectionBudget
	// named is the route the request names; every leg parses the request as
	// addressed to it, as the gateway parses it once.
	named     string
	supply    runtime.SupplyReader
	unhealthy func(providerID string) bool
	// key reads the selected API key's standing on a route, and eligibility
	// the credential eligibility of a route's targets.
	key         func(runtime.Route) (inspectionKeyContext, error)
	eligibility func(runtime.Route) (func(string) runtime.Eligibility, error)
}

// simulatedLeg is one route the request plans on.
type simulatedLeg struct {
	Route     string                    `json:"route"`
	Via       *string                   `json:"via"`
	Selectors []runtime.SelectorOutcome `json:"selectors"`
	Affinity  *simulatedAffinity        `json:"affinity"`
	Decisions []inspectedDecision       `json:"decisions"`
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
	Fallbacks []runtime.FallbackStep `json:"fallbacks"`
}

// newSimulation reads the shared state a simulation plans with once.
func (s *Server) newSimulation(ctx context.Context, snapshot *runtime.Snapshot, input simulationInput, inputs *usage.RoutingInputs, demand *runtime.TokenDemand) *simulation {
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
func (m *simulation) permitted(route runtime.Route) bool {
	key, err := m.key(route)
	return err == nil && key.reason == ""
}

// evaluate answers a selector's classifier from the labels the caller supplied.
// Plugin predicates are not simulated.
func (m *simulation) evaluate(selector runtime.Selector) runtime.PredicateResult {
	classifier := selector.When.Classifier
	if classifier == nil {
		return runtime.PredicateResult{Failure: "plugin_not_simulated"}
	}
	label, ok := m.input.ClassifierLabels[selector.ID]
	if !ok {
		return runtime.PredicateResult{Failure: "classifier_not_simulated"}
	}
	return runtime.PredicateResult{Matched: classifier.Accepts(label, nil), Label: label}
}

// leg plans the request on one route. The plan serves only when the key may
// use the route and it ranks at least one attempt.
func (s *Server) leg(m *simulation, slug, via string) (simulatedLeg, runtime.Plan, bool, error) {
	route := m.snapshot.Routes[slug]
	key, err := m.key(route)
	if err != nil {
		return simulatedLeg{}, runtime.Plan{}, false, err
	}
	eligibility, err := m.eligibility(route)
	if err != nil {
		return simulatedLeg{}, runtime.Plan{}, false, err
	}
	input := m.input
	semantic, err := inspectionContext(input.SemanticHeaders, input.QuerySettings, key.allowProviderState)
	if err != nil {
		return simulatedLeg{}, runtime.Plan{}, false, err
	}
	parsed, unary, mediaRequest, err := inspectorAnyRequest(input.Request, input.Operation, input.Surface, input.Mode, input.Dialect, cmp.Or(m.named, slug), route.Fidelity.Strict())
	if err != nil {
		return simulatedLeg{}, runtime.Plan{}, false, err
	}
	if input.Operation == "generation" {
		if err = inspectionClientContract(input.ClientContract, &semantic, s.Access.Keys != nil); err != nil {
			return simulatedLeg{}, runtime.Plan{}, false, err
		}
	}
	counted := newSimulatedDemand(parsed, input, m.demand)
	accept, effective, inspections := inspectionAccept(route, parsed, semantic, counted, &m.inspectionBudget)
	if unary != nil {
		accept, effective, inspections = inspectionUnaryAccept(route, *unary, semantic, input.ClientContract, m.demand, &m.inspectionBudget)
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
	options := runtime.SelectionOptions{
		Region:  s.Region,
		Context: m.ctx, KeyID: key.id, Preferences: preferences, Inputs: m.inputs, TokenDemand: counted.fixed, Demand: counted.sourceDemand(),
		CheckSlots: true, CredentialEligibility: eligibility, UnconfinedPlugins: s.UnconfinedPlugins,
		Accept: accept, Effective: effective, Supply: m.supply, Unhealthy: m.unhealthy,
	}
	if key.reason != "" {
		options.Accept = nil
		options.Effective = nil
	}
	if parsed != nil {
		options.Parameters = sync.OnceValue(func() []string { return protocols.ParameterNames(parsed) })
	}
	if len(route.Selectors) > 0 {
		options.Features = runtime.DescribeRequest(input.Operation, parsed, counted.request())
		options.Evaluate, options.Permitted = m.evaluate, m.permitted
	}
	if route.Shadowed() {
		options.Mirror = []byte(input.Seed)
	}
	plan, err := runtime.PlanRequest(m.snapshot, slug, input.Operation, input.Surface, input.Mode, route.Affinity.Seed(input.Attribution, parsed, []byte(input.Seed)), options)
	if err != nil {
		return simulatedLeg{}, runtime.Plan{}, false, err
	}
	applyInspectionKeyReason(plan.Decisions, key.reason)
	leg := simulatedLeg{
		Route: slug, Selectors: plan.Selectors,
		Decisions: inspectedDecisions(plan.Decisions, route, parsed != nil || unary != nil || mediaRequest != nil, inspections, counted.estimates),
	}
	if leg.Selectors == nil {
		leg.Selectors = []runtime.SelectorOutcome{}
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
	return leg, plan, key.reason == "", nil
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
	w.out = explanation{Legs: []simulatedLeg{first}, Fallbacks: []runtime.FallbackStep{}}
	if !permitted {
		return w.out, nil
	}
	if w.settle(plan) == runtime.FallbackPlanned || w.fallBack() {
		w.standby()
	}
	return w.out, nil
}

// walk is the route state of a simulated request as it moves between routes.
type walk struct {
	s          *Server
	m          *simulation
	route      *runtime.Route
	delegators []*runtime.Route
	visited    []string
	conditions []string
	frames     []walkFrame
	out        explanation
}

type walkFrame struct {
	route      *runtime.Route
	conditions []string
	next       int
}

// settle follows a plan's selector delegation and records the conditions an
// unserved plan names.
func (w *walk) settle(plan runtime.Plan) string {
	w.conditions = nil
	if plan.Delegate != "" {
		if w.adopt(plan.Delegate, usage.ViaSelector) != runtime.FallbackPlanned {
			return runtime.NoEligibleTargets
		}
		return runtime.FallbackPlanned
	}
	if len(plan.Attempts) == 0 {
		w.conditions = plan.Conditions()
		return runtime.NoEligibleTargets
	}
	return runtime.FallbackPlanned
}

// adopt moves the request to another route, as the gateway does.
func (w *walk) adopt(slug, via string) string {
	if slices.Contains(w.visited, slug) {
		return runtime.FallbackRepeated
	}
	route, ok := w.m.snapshot.Routes[slug]
	if !ok {
		return runtime.FallbackUnavailable
	}
	if !w.m.permitted(route) {
		return runtime.FallbackForbidden
	}
	if via == usage.ViaSelector {
		w.delegators = append(w.delegators, w.route)
	} else {
		w.delegators = nil
	}
	w.route, w.visited = &route, append(w.visited, slug)
	leg, plan, _, err := w.s.leg(w.m, slug, via)
	if err != nil {
		w.conditions = nil
		var refusal *runtime.SelectionError
		if errors.As(err, &refusal) {
			return refusal.Code
		}
		return runtime.NoEligibleTargets
	}
	w.out.Legs = append(w.out.Legs, leg)
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
		step := runtime.FallbackStep{From: from, Route: fallback.Route, Conditions: met}
		step.Outcome = w.adopt(fallback.Route, usage.ViaFallback)
		w.out.Fallbacks = append(w.out.Fallbacks, step)
		if step.Outcome == runtime.FallbackPlanned {
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
			step := runtime.FallbackStep{From: frame.route.Slug, Route: fallback.Route, Conditions: fallback.On, Outcome: fallbackStandby}
			target, ok := w.m.snapshot.Routes[fallback.Route]
			switch {
			case slices.Contains(w.visited, fallback.Route):
				step.Outcome = runtime.FallbackRepeated
			case !ok:
				step.Outcome = runtime.FallbackUnavailable
			case !w.m.permitted(target):
				step.Outcome = runtime.FallbackForbidden
			}
			w.out.Fallbacks = append(w.out.Fallbacks, step)
		}
	}
}
