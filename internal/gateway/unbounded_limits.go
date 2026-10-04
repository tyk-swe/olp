package gateway

import (
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
)

// Batch execution and Live sessions cannot bound their future token consumption
// or cost before dispatch. A small lifecycle reservation must never stand in
// for enforcement of these budgets. Request-rate and concurrency limits remain
// enforceable and are reserved by the normal admission path.
func unboundedWorkLimits(authority access.Authority, provider *runtime.Provider, slot *runtime.Slot) *Error {
	p := authority.Policy
	if p.TokensPerMinute != nil || p.DailyCostLimit != nil || p.MonthlyCostLimit != nil ||
		authority.BudgetGroupDailyCostLimit != nil || authority.BudgetGroupMonthlyCostLimit != nil ||
		(provider != nil && provider.Limits != nil && provider.Limits.TokensPerMinute != nil) ||
		(slot != nil && slot.TokensPerMinute != nil) {
		return invalidRequest("unbounded_work_budget", "This operation cannot enforce token or cost budgets; use a bounded inference operation.", nil)
	}
	return nil
}

// A retained file or an ongoing session must also respect consumption budgets
// added to the current release after its serving contract was pinned.
func (s *Server) unboundedPinLimits(authority access.Authority, p *pin) *Error {
	if e := unboundedWorkLimits(authority, &p.provider, &p.slot); e != nil {
		return e
	}
	if release := s.Runtime.Release(); release != nil {
		if provider, ok := release.Snapshot.Providers[p.provider.ID]; ok {
			for _, slot := range provider.Slots {
				if slot.ID == p.slot.ID {
					return unboundedWorkLimits(authority, &provider, &slot)
				}
			}
			return unboundedWorkLimits(authority, &provider, nil)
		}
	}
	return nil
}
