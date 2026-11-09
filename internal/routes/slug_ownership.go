package routes

import (
	"context"

	"github.com/tyk-swe/olp/internal/access"
)

func SlugsOwnedOutsideProject(ctx context.Context, q access.Queryer, slugs []string, project string) (bool, error) {
	var foreign bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM (SELECT slug,project_id FROM olp.routes UNION ALL SELECT slug,project_id FROM olp.code_routes) r LEFT JOIN olp.projects p ON p.id=r.project_id WHERE r.slug=ANY($1::text[]) AND lower(p.name) IS DISTINCT FROM lower($2::text))`, slugs, project).Scan(&foreign)
	return foreign, err
}
