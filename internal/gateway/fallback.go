package gateway

import (
	"context"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage"
)

// planner names how a request plans the route it is on. A request that moves
// to a fallback or selector route plans that route the same way.
type planner uint8

const (
	canonicalPlanner planner = iota
	unaryPlanner
	mediaPlanner
)

// fallbackFrame is a failed route whose declared fallbacks remain to be tried
// for the conditions it met.
type fallbackFrame struct {
	route      *routing.Route
	conditions []string
	next       int
}

// bind makes x.route the request's primary route: its deadline and attempt
// budget bound the request, whichever routes it moves on to. A fixed request
// belongs to its route and never leaves it.
func (x *call) bind(p planner, authorize func(*routing.Route) *Error, fixed bool) {
	x.primary, x.planner, x.authorize, x.fixed = x.route, p, authorize, fixed
	x.visited = append(x.visited[:0], x.route.Slug)
	x.seed = x.affinity
	x.affinity = x.routeSeed(x.route)
}

// named is the route the caller named.
func (x *call) named() *routing.Route {
	if x.primary != nil {
		return x.primary
	}
	return x.route
}

// planNamed plans the primary route. A route that cannot plan any attempt for
// a reason one of its fallbacks names moves the request on before dispatch.
func (s *Server) planNamed(ctx context.Context, x *call) *Error {
	ctx, cancel := x.routeContext(ctx)
	defer cancel()
	e := s.replan(ctx, x)
	if e != nil && len(x.planConditions) > 0 && s.fallBack(ctx, x, x.planConditions) {
		return nil
	}
	return e
}

// routeContext starts the named route's deadline once, before classifiers and
// other dynamic planning. Admission, dispatch and fallback share what remains.
func (x *call) routeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if x.deadline.IsZero() {
		timeout := time.Duration(x.named().OverallTimeout) * time.Millisecond
		if x.named().ToolExecution == "gateway" {
			timeout = min(timeout, 120*time.Second)
		}
		x.deadline = time.Now().Add(timeout)
	}
	return context.WithDeadline(ctx, x.deadline)
}

// replan plans x.route with the request's own planner.
func (s *Server) replan(ctx context.Context, x *call) *Error {
	x.planConditions = nil
	if e := x.checkBody(x.route); e != nil {
		return e
	}
	switch x.planner {
	case unaryPlanner:
		return s.planUnary(ctx, x)
	case mediaPlanner:
		return s.planMedia(ctx, x)
	}
	return s.planCanonical(ctx, x)
}

// adoptPlan takes the plan of the route the request is on. The primary
// route's attempt budget bounds the request, so a later route receives what
// remains of it.
func (x *call) initAllowance(budget int) {
	if x.allowance == 0 {
		x.allowance = x.spent + budget
	}
}

func (x *call) adoptPlan(plan routing.Plan) {
	x.initAllowance(plan.Budget)
	x.decisions, x.policy, x.attempts, x.shadows = plan.Decisions, plan.Policy, plan.Attempts, plan.Shadows
	x.finishPreparationPlanning()
	x.budget = x.spent + min(plan.Budget, x.allowance-x.spent)
	x.selector, x.baseline = plan.Selected(), nil
	if b := plan.Baseline; b != nil {
		x.baseline = &usage.Baseline{ProviderID: b.ProviderID, UpstreamModel: b.UpstreamModel, VendorID: optionalText(b.VendorID)}
	}
	if len(plan.Attempts) == 0 {
		x.planConditions = plan.Conditions()
	}
}

// delegate moves the request to the route its matched selector names. The
// planner has already checked that the route exists and the key may use it.
func (s *Server) delegate(ctx context.Context, x *call, plan routing.Plan) *Error {
	x.initAllowance(plan.Budget)
	selector := plan.Selected()
	outcome, e := s.adopt(ctx, x, plan.Delegate, usage.ViaSelector)
	if e != nil {
		return e
	}
	if outcome != routing.FallbackPlanned {
		if x.costExcluded {
			return costLimitExceeded()
		}
		return selectionError(&routing.SelectionError{Code: routing.NoEligibleTargets}, x.route.Slug)
	}
	if x.selector == "" {
		x.selector = selector
	}
	return nil
}

// adopt moves the request to the named route and plans it, returning the
// outcome a fallback step records and the route's planning refusal.
func (s *Server) adopt(ctx context.Context, x *call, slug, via string) (string, *Error) {
	if slices.Contains(x.visited, slug) {
		return routing.FallbackRepeated, nil
	}
	route, ok := x.snapshot().Routes[slug]
	if !ok {
		return routing.FallbackUnavailable, nil
	}
	if x.authorize(&route) != nil {
		return routing.FallbackForbidden, nil
	}
	x.useRoute(&route, via)
	if e := s.replan(ctx, x); e != nil {
		return e.Code, e
	}
	return routing.FallbackPlanned, nil
}

// fallBack moves a request whose route failed with conditions to the first
// declared fallback those conditions start. Fallbacks are tried depth first:
// a fallback route's own fallbacks come before its siblings, and a selector's
// route fails for the route that delegated to it as well. It reports whether
// a fallback route planned attempts within what remains of the request's
// attempt budget.
func (s *Server) fallBack(ctx context.Context, x *call, conditions []string) bool {
	if x.primary == nil || x.fixed {
		return false
	}
	x.pushFrames(conditions)
	for len(x.frames) > 0 && x.spent < x.allowance && ctx.Err() == nil {
		top := &x.frames[len(x.frames)-1]
		if top.next == len(top.route.Fallbacks) {
			x.frames = x.frames[:len(x.frames)-1]
			continue
		}
		fallback := top.route.Fallbacks[top.next]
		top.next++
		met := fallback.Met(top.conditions)
		if len(met) == 0 {
			continue
		}
		step := routing.FallbackStep{From: top.route.Slug, Route: fallback.Route, Conditions: met}
		step.Outcome, _ = s.adopt(ctx, x, fallback.Route, usage.ViaFallback)
		x.fallbacks = append(x.fallbacks, step)
		if step.Outcome == routing.FallbackPlanned {
			return true
		}
		if len(x.planConditions) > 0 {
			x.pushFrames(x.planConditions)
		}
	}
	return false
}

// pushFrames records that x.route, and every route that delegated to it,
// failed with conditions.
func (x *call) pushFrames(conditions []string) {
	push := func(route *routing.Route) {
		if len(route.Fallbacks) > 0 && !slices.ContainsFunc(x.frames, func(f fallbackFrame) bool { return f.route.Slug == route.Slug }) {
			x.frames = append(x.frames, fallbackFrame{route: route, conditions: conditions})
		}
	}
	for _, route := range x.delegators {
		push(route)
	}
	push(x.route)
}

// useRoute moves the request to route. Provider preparation depends on the
// route's fidelity and content policy, and the session seed on its affinity,
// so both start over.
func (x *call) useRoute(route *routing.Route, via string) {
	if via == usage.ViaSelector {
		x.delegators = append(x.delegators, x.route)
	} else {
		x.delegators = nil
	}
	x.routeLeg = routeLeg{route: route}
	x.visited = append(x.visited, route.Slug)
	x.leg = &usage.RouteLeg{Route: route.Slug, RevisionID: route.RevisionID, Via: via}
	x.requestPreparation = requestPreparation{}
	x.affinity = x.routeSeed(route)
}

// routeSeed is the rendezvous seed for route. A route with session affinity
// seeds selection with the request's session key, so requests of one session
// keep reaching the target and slot that hold their prompt cache; any other
// route keeps the seed the request's surface chose.
func (x *call) routeSeed(route *routing.Route) []byte {
	return route.Affinity.Seed(x.attribution, x.generation.parsed, x.seed)
}

// legPreferences are the caller's preferences for planning x.route. A caller's
// attempt budget is judged against the primary route only; later routes
// share what remains of it.
func (x *call) legPreferences() *routing.Preferences {
	if x.preferences == nil || x.preferences.MaxAttempts == nil || x.route == x.primary {
		return x.preferences
	}
	preferences := *x.preferences
	preferences.MaxAttempts = nil
	return &preferences
}

// mayLeaveRoute reports whether the request can move on to another route
// after its own attempts fail, so admission reserves its whole budget.
func (x *call) mayLeaveRoute() bool {
	return x.primary != nil && !x.fixed && (len(x.route.Fallbacks) > 0 || len(x.delegators) > 0 || len(x.frames) > 0)
}

// keyAuthorizer allows the routes an API key may use for inference.
func (s *Server) keyAuthorizer(authority access.Authority) func(*routing.Route) *Error {
	return func(route *routing.Route) *Error {
		if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
			return permissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`.")
		}
		return nil
	}
}

// fallbackCondition names the fallback condition a failure class meets.
func fallbackCondition(class string) string {
	switch class {
	case classRateLimit, classContextWindow, classContentFilter, classBudget:
		return class
	}
	return ""
}
