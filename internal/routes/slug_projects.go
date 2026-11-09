package routes

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

func SlugProjectIDs(ctx context.Context, q access.Queryer, slugs []string) ([]*string, error) {
	rows, err := q.Query(ctx, `SELECT r.project_id::text FROM (SELECT slug,project_id FROM olp.routes UNION ALL SELECT slug,project_id FROM olp.code_routes) r WHERE r.slug=ANY($1::text[])`, slugs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[*string])
}
