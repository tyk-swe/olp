package routes

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routing"
)

type StoredCodeRoute struct {
	ID          string
	Project     string
	Slug        string
	Draft       codemode.Route
	Published   *codemode.Route
	Connections map[string]providers.Configuration
	Paid        map[string]routing.Provider
}

type CodeRouteState struct {
	Draft       codemode.Route
	Published   *codemode.Route
	Connections map[string]providers.Configuration
	Paid        map[string]routing.Provider
}

func ReadStoredCodeRoutes(ctx context.Context, q access.Queryer) ([]StoredCodeRoute, error) {
	rows, err := q.Query(ctx, `SELECT x.id::text,p.name,x.slug,x.draft,v.document,coalesce((SELECT jsonb_object_agg(p.name,c.value) FROM jsonb_each(v.connections) c JOIN olp.providers p ON p.id::text=c.key),'{}'),coalesce(v.paid_connections,'{}') FROM olp.code_routes x JOIN olp.projects p ON p.id=x.project_id LEFT JOIN olp.code_route_revisions v ON v.id=x.latest_revision_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[StoredCodeRoute])
}

func ReadCodeRouteState(ctx context.Context, q access.Queryer, id string) (CodeRouteState, error) {
	var state CodeRouteState
	err := q.QueryRow(ctx, `SELECT x.draft,v.document,coalesce(v.connections,'{}'),coalesce(v.paid_connections,'{}') FROM olp.code_routes x LEFT JOIN olp.code_route_revisions v ON v.id=x.latest_revision_id WHERE x.id=$1`, id).Scan(&state.Draft, &state.Published, &state.Connections, &state.Paid)
	return state, err
}

func CodeSlugTaken(ctx context.Context, q access.Queryer, slug, projectID string) (bool, error) {
	var taken bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.routes WHERE slug=$1) OR EXISTS(SELECT 1 FROM olp.route_drafts WHERE slug=$1) OR EXISTS(SELECT 1 FROM olp.code_routes WHERE slug=$1 AND project_id<>$2)`, slug, projectID).Scan(&taken)
	return taken, err
}

func CodeRouteIDs(ctx context.Context, q access.Queryer, slug, projectID string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT id::text FROM olp.code_routes WHERE slug=$1 AND project_id=$2`, slug, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
