package projects

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type Named struct {
	ID   string
	Name string
}

func ReadProjectNames(ctx context.Context, q access.Queryer) ([]Named, error) {
	return readNamed(ctx, q, "SELECT id::text,name FROM olp.projects")
}

func ReadOrganizationNames(ctx context.Context, q access.Queryer) ([]Named, error) {
	return readNamed(ctx, q, "SELECT id::text,name FROM olp.organizations")
}

func readNamed(ctx context.Context, q access.Queryer, query string) ([]Named, error) {
	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Named])
}
