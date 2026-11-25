package apikeys

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits/ledger"
)

func ensureRouteBudgetAccounts(ctx context.Context, tx pgx.Tx, id string, policies keyauthority.RouteLimits) error {
	for route, policy := range policies {
		if policy.CostBudgeted() {
			if err := ledger.EnsureKeyRouteBudget(ctx, tx, id, route); err != nil {
				return err
			}
		}
	}
	return nil
}
