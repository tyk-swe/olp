package access

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/scim"
)

type SCIMProjectGrant struct {
	Project string `json:"project"`
	Role    string `json:"role"`
}

type SCIMGroupMapping struct {
	Name        string
	Role        *string
	AccessScope string
	Projects    []SCIMProjectGrant
}

func ReadSCIMGroupMappings(ctx context.Context, q Queryer) ([]SCIMGroupMapping, error) {
	rows, err := q.Query(ctx, `SELECT g.document->>'displayName',g.document->'`+scim.GroupExtension+`'->>'role',COALESCE(g.document->'`+scim.GroupExtension+`'->>'accessScope','assigned'),COALESCE((SELECT jsonb_agg(jsonb_build_object('project',p.name,'role',m.role) ORDER BY p.name) FROM olp.scim_group_projects m JOIN olp.projects p ON p.id=m.project_id WHERE m.group_id=g.id),'[]'::jsonb) FROM olp.scim_groups g ORDER BY lower(g.document->>'displayName')`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[SCIMGroupMapping])
}
