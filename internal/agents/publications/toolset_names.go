package publications

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

func ToolsetName(ctx context.Context, q access.Queryer, id string) (string, error) {
	var name string
	err := q.QueryRow(ctx, "SELECT name FROM olp.agent_publications WHERE id=$1 AND kind='toolset'", id).Scan(&name)
	return name, err
}

func FindLiveToolsetID(ctx context.Context, q access.Queryer, projectID *string, name string) (string, bool, error) {
	var id string
	err := q.QueryRow(ctx, "SELECT id::text FROM olp.agent_publications WHERE project_id IS NOT DISTINCT FROM $1::uuid AND kind='toolset' AND lower(name)=lower($2) AND NOT retired", projectID, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}
