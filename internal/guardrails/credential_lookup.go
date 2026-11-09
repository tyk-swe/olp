package guardrails

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

func FindCredentialID(ctx context.Context, q access.Queryer, projectID *string, name string) (string, bool, error) {
	var id string
	err := q.QueryRow(ctx, "SELECT id::text FROM olp.guardrail_credentials WHERE project_id IS NOT DISTINCT FROM $1::uuid AND name=$2", projectID, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

func CredentialName(ctx context.Context, q access.Queryer, id string) (string, error) {
	var name string
	err := q.QueryRow(ctx, "SELECT name FROM olp.guardrail_credentials WHERE id=$1", id).Scan(&name)
	return name, err
}
