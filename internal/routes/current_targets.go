package routes

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

func CurrentTargetIDs(ctx context.Context, q access.Queryer, slug, projectID, providerID, providerModel string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT t->>'id' FROM (
 SELECT d.targets,0 AS priority FROM olp.route_drafts d WHERE d.slug=$1 AND d.project_id=$2 AND d.state='draft'
 UNION ALL SELECT v.targets,1 AS priority FROM olp.routes r JOIN olp.route_revisions v ON v.id=r.latest_revision_id WHERE r.slug=$1 AND r.project_id=$2 AND r.state='active'
 ) r CROSS JOIN LATERAL jsonb_array_elements(r.targets) t
 WHERE (t->>'provider_id')::uuid=$3::uuid AND t->>'provider_model'=$4 AND r.priority=(SELECT min(priority) FROM (
 SELECT 0 AS priority FROM olp.route_drafts d WHERE d.slug=$1 AND d.project_id=$2 AND d.state='draft'
 UNION ALL SELECT 1 FROM olp.routes r WHERE r.slug=$1 AND r.project_id=$2 AND r.state='active') priorities)`, slug, projectID, providerID, providerModel)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
