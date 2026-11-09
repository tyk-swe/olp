package projects

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type ProjectPolicies struct {
	ID           string
	Name         string
	Organization *string
	Policies     Policies
}

func ReadProjectPolicies(ctx context.Context, q access.Queryer) ([]ProjectPolicies, error) {
	rows, err := q.Query(ctx, `SELECT id::text,name,(SELECT name FROM olp.organizations WHERE id=organization_id),end_user_policy,attribution_policy,route_groups,budget_policy,limit_templates,attribution_budgets FROM olp.projects`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ProjectPolicies, error) {
		var project ProjectPolicies
		err := row.Scan(&project.ID, &project.Name, &project.Organization, &project.Policies.EndUser, &project.Policies.Attribution, &project.Policies.RouteGroups, &project.Policies.Budget, &project.Policies.Templates, &project.Policies.AttributionBudgets)
		return project, err
	})
}
