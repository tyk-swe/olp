package gateway

import (
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/routing"
)

// callerCostBudgeted checks the boundaries this request actually spends against.
// A policy for another route or end user must not quarantine unlimited traffic.
func callerCostBudgeted(authority *access.Authority, route string) bool {
	return authority.CostBudgetedForRoute(route)
}

// Batch jobs and Live sessions cannot bound their future consumption before
// dispatch. Their lifecycle allowance cannot enforce a token or cost budget.
// Request-rate and concurrency limits remain supported by normal admission.
func unboundedWorkLimits(authority access.Authority, provider *routing.Provider, slot *routing.Slot, route *routing.Route) *Error {
	var slug string
	if route != nil {
		slug = route.Slug
	}
	if authority.UnboundedWorkBudgeted(slug) || routing.UnboundedWorkBudgeted(provider, slot, route) {
		return invalidRequest("unbounded_work_budget", "This operation cannot enforce token or cost budgets; use a bounded inference operation.", nil)
	}
	return nil
}

// A retained file and an ongoing session also respect consumption budgets added
// after the serving contract was pinned, including current route and slot caps.
func (s *Server) unboundedPinLimits(authority access.Authority, p *pin, route *routing.Route) *Error {
	if e := unboundedWorkLimits(authority, &p.provider, &p.slot, route); e != nil {
		return e
	}
	if release := s.Runtime.Release(); release != nil {
		if route != nil {
			if current, ok := release.Snapshot.Routes[route.Slug]; ok {
				route = &current
			}
		}
		if provider, ok := release.Snapshot.Providers[p.provider.ID]; ok {
			for _, slot := range provider.Slots {
				if slot.ID == p.slot.ID {
					return unboundedWorkLimits(authority, &provider, &slot, route)
				}
			}
			return unboundedWorkLimits(authority, &provider, nil, route)
		}
		return unboundedWorkLimits(authority, nil, nil, route)
	}
	return nil
}
