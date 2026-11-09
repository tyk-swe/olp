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
	rows, err := q.Query(ctx, "SELECT id::text,name,(SELECT name FROM olp.organizations WHERE id=organization_id),"+policyColumns+" FROM olp.projects")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ProjectPolicies, error) {
		var project ProjectPolicies
		err := row.Scan(append([]any{&project.ID, &project.Name, &project.Organization}, project.Policies.targets()...)...)
		return project, err
	})
}
