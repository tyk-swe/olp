package kernel

import (
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/routing"
)

// callerCostBudgeted checks the boundaries this request actually spends against.
// A policy for another route or end user must not quarantine unlimited traffic.
func callerCostBudgeted(authority *keyauthority.Authority, route string) bool {
	return authority.CostBudgetedForRoute(route)
}

// Batch jobs, Live sessions and Decisions cannot bound their future consumption
// before dispatch. Their lifecycle allowance cannot enforce a token or cost budget.
// Request-rate and concurrency limits remain supported by normal admission.
// Caller-paid exempt routes waive cost caps on caller-credential providers only.
// callerRoute retains named-route key limits when a selector changes route.
func UnboundedWorkLimits(authority keyauthority.Authority, provider *routing.Provider, slot *routing.Slot, route *routing.Route, callerRoute ...string) *fault.Error {
	var slug string
	if route != nil {
		slug = route.Slug
	}
	if len(callerRoute) > 0 {
		slug = callerRoute[0]
	}
	costExempt := route != nil && route.CallerCostExempt && (provider == nil || provider.CredentialSource == "caller")
	if authority.UnboundedWorkBudgeted(slug, costExempt) || routing.UnboundedWorkBudgeted(provider, slot, route, costExempt) {
		return unboundedWorkBudget()
	}
	return nil
}

func unboundedWorkBudget() *fault.Error {
	return fault.InvalidRequest("unbounded_work_budget", "This operation cannot enforce token or cost budgets; use a bounded inference operation.", nil)
}

// A retained file and an ongoing session also respect consumption budgets added
// after the serving contract was pinned, including current route and slot caps.
func (s *Server) UnboundedPinLimits(authority keyauthority.Authority, p *Pin, route *routing.Route) *fault.Error {
	return s.unboundedPinLimits(authority, p, route, "")
}

func (s *Server) unboundedPinLimits(authority keyauthority.Authority, p *Pin, route *routing.Route, model string) *fault.Error {
	if _, _, limited := p.Provider.SharedTokenQuotaFor(model); limited {
		return unboundedWorkBudget()
	}
	if e := UnboundedWorkLimits(authority, &p.Provider, &p.Slot, route); e != nil {
		return e
	}
	if s.Deps.Runtime == nil {
		return nil
	}
	release := s.Deps.Runtime.Release()
	if release == nil {
		return nil
	}
	snapshot := release.Snapshot()
	currentRoute := route
	if route != nil {
		if current, ok := snapshot.FindRoute(route.Slug); ok {
			currentRoute = &current
		}
	}
	provider, ok := snapshot.FindProvider(p.Provider.ID)
	if !ok {
		return UnboundedWorkLimits(authority, nil, nil, currentRoute)
	}
	if _, _, limited := provider.SharedTokenQuotaFor(model); limited {
		return unboundedWorkBudget()
	}
	for _, slot := range provider.Slots {
		if slot.ID == p.Slot.ID {
			return UnboundedWorkLimits(authority, &provider, &slot, currentRoute)
		}
	}
	return UnboundedWorkLimits(authority, &provider, nil, currentRoute)
}
