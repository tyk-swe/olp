package access

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

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
