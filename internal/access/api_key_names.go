package access

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type NamedAPIKey struct {
	Name      string
	ProjectID string
}

func APIKeyName(ctx context.Context, q Queryer, id string) (NamedAPIKey, error) {
	var key NamedAPIKey
	err := q.QueryRow(ctx, "SELECT name,COALESCE(project_id::text,'installation') FROM olp.api_keys WHERE id=$1", id).Scan(&key.Name, &key.ProjectID)
	return key, err
}

func ReadLiveAPIKeyIDs(ctx context.Context, q Queryer, projectID *string, name string) ([]string, error) {
	rows, err := q.Query(ctx, "SELECT id::text FROM olp.api_keys WHERE project_id IS NOT DISTINCT FROM $1::uuid AND name=$2 AND revoked_at IS NULL", projectID, name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
