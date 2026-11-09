package access

import (
	"context"
	"errors"

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

func ActiveKeyIDsNamed(ctx context.Context, q Queryer, projectID, name string) ([]string, error) {
	rows, err := q.Query(ctx, "SELECT id::text FROM olp.api_keys WHERE project_id=$1 AND name=$2 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now())", projectID, name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func ActiveKeyName(ctx context.Context, q Queryer, id, projectID string) (string, bool, error) {
	var name string
	err := q.QueryRow(ctx, "SELECT name FROM olp.api_keys WHERE id=$1 AND project_id=$2 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now())", id, projectID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return name, err == nil, err
}
