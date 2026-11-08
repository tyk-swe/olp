package modelcatalog

import (
	"context"
	"maps"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type projectPublication struct {
	project     string
	publication Publication
}

func ReadPublications(ctx context.Context, q access.Queryer) (map[string]Publication, error) {
	rows, err := q.Query(ctx, `SELECT p.id::text,COALESCE(c.enabled,false),COALESCE(c.prices_public,false),COALESCE(c.etag,p.etag)::text FROM olp.projects p LEFT JOIN olp.model_catalog_project_settings c ON c.project_id=p.id`)
	if err != nil {
		return nil, err
	}
	publications, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (projectPublication, error) {
		var scanned projectPublication
		err := row.Scan(&scanned.project, &scanned.publication.Enabled, &scanned.publication.PricesPublic, &scanned.publication.ETag)
		return scanned, err
	})
	if err != nil {
		return nil, err
	}
	return byProject(publications), nil
}

func byProject(publications []projectPublication) map[string]Publication {
	return maps.Collect(func(yield func(string, Publication) bool) {
		for _, scanned := range publications {
			if !yield(scanned.project, scanned.publication) {
				return
			}
		}
	})
}

func SetProjectPublication(ctx context.Context, tx pgx.Tx, project string, enabled, pricesPublic bool) (Publication, error) {
	publication := Publication{Enabled: enabled, PricesPublic: pricesPublic, ETag: access.NewID()}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.model_catalog_project_settings(project_id,enabled,prices_public,etag) VALUES($1,$2,$3,$4) ON CONFLICT(project_id) DO UPDATE SET enabled=excluded.enabled,prices_public=excluded.prices_public,etag=excluded.etag`, project, publication.Enabled, publication.PricesPublic, publication.ETag); err != nil {
		return Publication{}, err
	}
	return publication, nil
}

func SetRoutePublication(ctx context.Context, tx pgx.Tx, routeID string, exposeUpstreamModels bool) (Exposure, error) {
	exposure := Exposure{ExposeUpstreamModels: exposeUpstreamModels, ETag: access.NewID()}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.model_catalog_route_settings(route_id,expose_upstream_models,etag) VALUES($1,$2,$3) ON CONFLICT(route_id) DO UPDATE SET expose_upstream_models=excluded.expose_upstream_models,etag=excluded.etag`, routeID, exposure.ExposeUpstreamModels, exposure.ETag); err != nil {
		return Exposure{}, err
	}
	return exposure, nil
}
