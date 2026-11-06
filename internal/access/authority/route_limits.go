package authority

import (
	"github.com/tyk-swe/olp/internal/routeidentity"
)

type RouteLimits map[string]AdmissionLimits

func (policies RouteLimits) ValidateAndNormalize() error {
	if len(policies) > 100 {
		return invalid("route_limits", "Use at most 100 routes.")
	}
	for route, policy := range policies {
		if !routeidentity.ValidSlug(route) {
			return invalid("route_limits", "Each entry must name a route slug.")
		}
		if err := policy.ValidateAndNormalize(); err != nil {
			return err
		}
	}
	return nil
}

func (policies RouteLimits) Limited() bool {
	for _, p := range policies {
		if p.Limited() {
			return true
		}
	}
	return false
}

func (policies RouteLimits) CostBudgeted() bool {
	for _, p := range policies {
		if p.CostBudgeted() {
			return true
		}
	}
	return false
}
