package access

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/limits"
)

type RouteLimits map[string]AdmissionLimits

func (policies RouteLimits) Validate() error {
	if len(policies) > 100 {
		return Invalid("route_limits", "Use at most 100 routes.")
	}
	for route, policy := range policies {
		if !RouteSlug.MatchString(route) {
			return Invalid("route_limits", "Each entry must name a route slug.")
		}
		if err := policy.Validate(); err != nil {
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
func ensureRouteBudgetAccounts(ctx context.Context, tx pgx.Tx, id string, policies RouteLimits) error {
	for route, policy := range policies {
		if policy.CostBudgeted() {
			if err := limits.EnsureKeyRouteBudget(ctx, tx, id, route); err != nil {
				return err
			}
		}
	}
	return nil
}
