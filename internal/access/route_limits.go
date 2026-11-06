package access

import (
	"context"

	"github.com/jackc/pgx/v5"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/limits"
)

func ensureRouteBudgetAccounts(ctx context.Context, tx pgx.Tx, id string, policies keyauthority.RouteLimits) error {
	for route, policy := range policies {
		if policy.CostBudgeted() {
			if err := limits.EnsureKeyRouteBudget(ctx, tx, id, route); err != nil {
				return err
			}
		}
	}
	return nil
}
