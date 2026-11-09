package access

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

type StoredKey struct {
	Name      string
	ProjectID *string
	Revoked   bool
	Expired   bool
}

type NamedKey struct {
	ID      string
	Expired bool
}

func ReadAPIKey(ctx context.Context, q Queryer, id string) (StoredKey, bool, error) {
	var key StoredKey
	err := q.QueryRow(ctx, "SELECT name,project_id::text,revoked_at IS NOT NULL,expires_at IS NOT NULL AND expires_at<=now() FROM olp.api_keys WHERE id=$1", id).Scan(&key.Name, &key.ProjectID, &key.Revoked, &key.Expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredKey{}, false, nil
	}
	return key, err == nil, err
}

func ReadKeysNamed(ctx context.Context, q Queryer, projectID *string, name string) ([]NamedKey, error) {
	rows, err := q.Query(ctx, "SELECT id::text,expires_at IS NOT NULL AND expires_at<=now() FROM olp.api_keys WHERE project_id IS NOT DISTINCT FROM $1::uuid AND name=$2 AND revoked_at IS NULL", projectID, name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[NamedKey])
}

func ProjectKeyUsable(ctx context.Context, q Queryer, keyID, projectID string, createdBy *string) (bool, error) {
	var usable bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.api_keys WHERE id=$1 AND project_id=$2 AND revoked_at IS NULL AND ($3::uuid IS NULL OR created_by=$3))`, keyID, projectID, createdBy).Scan(&usable)
	return usable, err
}
