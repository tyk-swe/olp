package routes

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type SlugOwner struct {
	RouteID   string
	ProjectID *string
}

func RouteSlug(ctx context.Context, q access.Queryer, id string) (string, error) {
	var slug string
	err := q.QueryRow(ctx, "SELECT slug FROM olp.routes WHERE id=$1", id).Scan(&slug)
	return slug, err
}

func RouteBySlug(ctx context.Context, q access.Queryer, slug string) (SlugOwner, error) {
	var owner SlugOwner
	err := q.QueryRow(ctx, "SELECT id::text,project_id::text FROM olp.routes WHERE slug=$1", slug).Scan(&owner.RouteID, &owner.ProjectID)
	return owner, err
}

func FindLatestDraftID(ctx context.Context, q access.Queryer, slug string, projectID *string) (string, bool, error) {
	var id string
	err := q.QueryRow(ctx, "SELECT id::text FROM olp.route_drafts WHERE slug=$1 AND project_id IS NOT DISTINCT FROM $2::uuid ORDER BY updated_at DESC LIMIT 1", slug, projectID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}
