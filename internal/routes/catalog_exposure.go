package routes

import (
	"context"

	"github.com/jackc/pgx/v5"
)

func SetCatalogExposure(ctx context.Context, tx pgx.Tx, routeID string, exposeUpstreamModels bool, etag string) error {
	_, err := tx.Exec(ctx, `INSERT INTO olp.model_catalog_route_settings(route_id,expose_upstream_models,etag) VALUES($1,$2,$3) ON CONFLICT(route_id) DO UPDATE SET expose_upstream_models=excluded.expose_upstream_models,etag=excluded.etag`, routeID, exposeUpstreamModels, etag)
	return err
}
