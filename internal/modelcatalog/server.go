package modelcatalog

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/runtime"
)

type Server struct {
	Access  *access.Server
	Runtime *runtime.Manager
	Gateway *gateway.Server
	Origin  string
}

type Publication struct {
	Enabled      bool   `json:"enabled"`
	PricesPublic bool   `json:"prices_public"`
	ETag         string `json:"etag"`
}

type Exposure struct {
	ExposeUpstreamModels bool   `json:"expose_upstream_models"`
	ETag                 string `json:"etag"`
}

func (s *Server) Register(mux *http.ServeMux) {
	s.Access.Route(mux, "GET /api/v1/catalog", s.list, access.InferenceKeyAuth(func(r *http.Request) (access.Authority, error) {
		if s.Gateway == nil || s.Runtime == nil {
			return access.Authority{}, access.Fail(503, "authority_unavailable", "Key authority is unavailable.")
		}
		return s.Gateway.CatalogAuthority(r)
	}))
	s.Access.Public(mux, "GET /api/v1/catalog/public/{project_id}", s.public)
	s.Access.Route(mux, "GET /api/v1/projects/{project_id}/catalog", s.getPublication)
	s.Access.Route(mux, "PUT /api/v1/projects/{project_id}/catalog", s.putPublication)
	s.Access.Route(mux, "GET /api/v1/routes/{route_id}/catalog", s.getExposure)
	s.Access.Route(mux, "PUT /api/v1/routes/{route_id}/catalog", s.putExposure)
}

func (s *Server) list(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.describe(r, func(route runtime.Route) bool {
		if p.Kind == "key" {
			return p.KeyAuthority != nil && p.KeyAuthority.Allows("models_read", route.Slug, route.ProjectID, time.Now())
		}
		return p.Project(route.ProjectID, access.View) == nil
	}, true)
}

func (s *Server) public(r *http.Request) (access.Reply, error) {
	id, err := access.IDParam(r, "project_id")
	if err != nil {
		return access.Reply{}, err
	}
	publication, err := readPublication(r.Context(), s.Access.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	if !publication.Enabled {
		return access.Reply{}, access.Fail(404, "not_found", "The resource was not found.")
	}
	return s.describe(r, func(route runtime.Route) bool { return route.ProjectID != nil && *route.ProjectID == id }, publication.PricesPublic)
}

func (s *Server) describe(r *http.Request, visible func(runtime.Route) bool, prices bool) (access.Reply, error) {
	if s.Runtime == nil {
		return access.Reply{}, access.Fail(503, "catalog_unavailable", "The catalog is unavailable.")
	}
	release := s.Runtime.Release()
	if release == nil || release.Snapshot == nil {
		return access.Reply{}, access.Fail(503, "catalog_unavailable", "The catalog is unavailable.")
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT route_id::text,expose_upstream_models FROM olp.model_catalog_route_settings")
	if err != nil {
		return access.Reply{}, err
	}
	exposure := map[string]bool{}
	for rows.Next() {
		var id string
		var exposed bool
		if err = rows.Scan(&id, &exposed); err != nil {
			rows.Close()
			return access.Reply{}, err
		}
		exposure[id] = exposed
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	models := []Model{}
	inputs, now := s.Runtime.RoutingInputs(), time.Now()
	for _, route := range release.Snapshot.Routes {
		if visible(route) {
			models = append(models, Describe(release.Snapshot, route, inputs, s.Origin, prices, exposure[route.ID], now))
		}
	}
	slices.SortFunc(models, func(a, b Model) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return access.OK(map[string]any{"items": models, "prices_visible": prices}), nil
}

func readPublication(ctx context.Context, q access.Queryer, id string) (Publication, error) {
	var result Publication
	err := q.QueryRow(ctx, `SELECT COALESCE(c.enabled,false),COALESCE(c.prices_public,false),COALESCE(c.etag,p.etag)::text FROM olp.projects p LEFT JOIN olp.model_catalog_project_settings c ON c.project_id=p.id WHERE p.id=$1`, id).Scan(&result.Enabled, &result.PricesPublic, &result.ETag)
	if errors.Is(err, pgx.ErrNoRows) {
		err = access.Fail(404, "not_found", "The resource was not found.")
	}
	return result, err
}

func (s *Server) getPublication(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "project_id")
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&id, access.View); err != nil {
		return access.Reply{}, err
	}
	publication, err := readPublication(r.Context(), s.Access.Pool, id)
	return access.Detail(publication, publication.ETag), err
}

func (s *Server) putPublication(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "project_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input struct {
		Enabled      *bool `json:"enabled"`
		PricesPublic *bool `json:"prices_public"`
	}
	if err = access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	if input.Enabled == nil || input.PricesPublic == nil {
		return access.Reply{}, access.Invalid("enabled", "Include enabled and prices_public explicitly.")
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&id, access.Change); err != nil {
		return access.Reply{}, err
	}
	var locked string
	if err = tx.QueryRow(r.Context(), "SELECT id::text FROM olp.projects WHERE id=$1 FOR UPDATE", id).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
		return access.Reply{}, access.Fail(404, "not_found", "The resource was not found.")
	} else if err != nil {
		return access.Reply{}, err
	}
	current, err := readPublication(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	result := Publication{Enabled: *input.Enabled, PricesPublic: *input.PricesPublic, ETag: access.NewID()}
	_, err = tx.Exec(r.Context(), `INSERT INTO olp.model_catalog_project_settings(project_id,enabled,prices_public,etag) VALUES($1,$2,$3,$4) ON CONFLICT(project_id) DO UPDATE SET enabled=excluded.enabled,prices_public=excluded.prices_public,etag=excluded.etag`, id, result.Enabled, result.PricesPublic, result.ETag)
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "catalog.publish", "project", id, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Detail(result, result.ETag))
}

func readExposure(ctx context.Context, q access.Queryer, id string) (Exposure, *string, error) {
	var result Exposure
	var project *string
	err := q.QueryRow(ctx, `SELECT COALESCE(c.expose_upstream_models,false),COALESCE(c.etag,r.etag)::text,r.project_id::text FROM olp.routes r LEFT JOIN olp.model_catalog_route_settings c ON c.route_id=r.id WHERE r.id=$1 AND r.state='active'`, id).Scan(&result.ExposeUpstreamModels, &result.ETag, &project)
	if errors.Is(err, pgx.ErrNoRows) {
		err = access.Fail(404, "not_found", "The resource was not found.")
	}
	return result, project, err
}

func (s *Server) getExposure(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "route_id")
	if err != nil {
		return access.Reply{}, err
	}
	result, project, err := readExposure(r.Context(), s.Access.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(project, access.View); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(result, result.ETag), nil
}

func (s *Server) putExposure(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "route_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input struct {
		ExposeUpstreamModels *bool `json:"expose_upstream_models"`
	}
	if err = access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	if input.ExposeUpstreamModels == nil {
		return access.Reply{}, access.Invalid("expose_upstream_models", "Include expose_upstream_models explicitly.")
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	var locked string
	if err = tx.QueryRow(r.Context(), "SELECT id::text FROM olp.routes WHERE id=$1 AND state='active' FOR UPDATE", id).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
		return access.Reply{}, access.Fail(404, "not_found", "The resource was not found.")
	} else if err != nil {
		return access.Reply{}, err
	}
	current, project, err := readExposure(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(project, access.Change); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	result := Exposure{ExposeUpstreamModels: *input.ExposeUpstreamModels, ETag: access.NewID()}
	_, err = tx.Exec(r.Context(), `INSERT INTO olp.model_catalog_route_settings(route_id,expose_upstream_models,etag) VALUES($1,$2,$3) ON CONFLICT(route_id) DO UPDATE SET expose_upstream_models=excluded.expose_upstream_models,etag=excluded.etag`, id, result.ExposeUpstreamModels, result.ETag)
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "catalog.exposure", "route", id, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Detail(result, result.ETag))
}
