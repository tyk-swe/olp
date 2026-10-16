package gateway

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
)

type CacheManagement struct {
	Access  *access.Server
	Gateway *Server
}

func (s *CacheManagement) Register(mux *http.ServeMux) {
	s.Access.Route(mux, "POST /api/v1/cache/purge", s.purge)
	s.Access.Route(mux, "GET /api/v1/cache/statistics", s.statistics)
}
func (s *CacheManagement) purge(r *http.Request, p access.Principal) (access.Reply, error) {
	var in struct {
		RouteID   *string `json:"route_id"`
		ProjectID *string `json:"project_id"`
	}
	if err := access.Decode(r, &in); err != nil {
		return access.Reply{}, err
	}
	if (in.RouteID == nil) == (in.ProjectID == nil) {
		return access.Reply{}, access.Invalid("scope", "Supply exactly one route_id or project_id.")
	}
	if s.Gateway == nil || s.Gateway.Cache == nil {
		return access.Reply{}, access.Fail(503, "cache_unavailable", "The response cache is unavailable.")
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err = s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	kind, id := "project", in.ProjectID
	project := in.ProjectID
	if in.RouteID != nil {
		kind, id = "route", in.RouteID
		if _, err = uuid.Parse(*id); err != nil {
			return access.Reply{}, access.Invalid("route_id", "Use a route UUID.")
		}
		if err = tx.QueryRow(r.Context(), "SELECT project_id::text FROM olp.routes WHERE id=$1", *id).Scan(&project); err != nil {
			return access.Reply{}, err
		}
	}
	if kind == "project" {
		if _, err = uuid.Parse(*id); err != nil {
			return access.Reply{}, access.Invalid("project_id", "Use a project UUID.")
		}
		if err = s.Access.RequireProject(r.Context(), tx, p, project); err != nil {
			return access.Reply{}, err
		}
	}
	if err = p.Project(project, access.Change); err != nil {
		return access.Reply{}, err
	}
	if err = s.Gateway.Cache.Purge(r.Context(), kind, *id); err != nil {
		return access.Reply{}, access.Fail(503, "cache_unavailable", "The response cache is unavailable.")
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "cache.purge", kind, *id, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.OK(map[string]bool{"purged": true}))
}

func (s *CacheManagement) statistics(r *http.Request, p access.Principal) (access.Reply, error) {
	route := r.URL.Query().Get("route")
	if route != "" && !access.RouteSlug.MatchString(route) {
		return access.Reply{}, access.Invalid("route", "Use a route slug.")
	}
	var requests, hits, misses, bypasses int64
	const bounds = ` FROM olp.requests r JOIN olp.api_keys k ON k.id=r.api_key_id WHERE r.started_at>=now()-interval '30 days' AND ($1 OR k.project_id=ANY($2::uuid[])) AND ($3='' OR r.route_slug=$3)`
	err := s.Access.Pool.QueryRow(r.Context(), `SELECT count(*) FILTER (WHERE r.cache_result IS NOT NULL),count(*) FILTER (WHERE r.cache_result IN ('hit','semantic-hit')),count(*) FILTER (WHERE r.cache_result='miss'),count(*) FILTER (WHERE r.cache_result='bypass')`+bounds, p.AllProjects, p.ProjectIDs(), route).Scan(&requests, &hits, &misses, &bypasses)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT COALESCE(f.currency,''),sum(f.cache_avoided_cost)::text FROM olp.attempt_usage_facts f JOIN olp.requests r ON r.id=f.request_id AND r.started_at=f.request_started_at JOIN olp.api_keys k ON k.id=r.api_key_id WHERE f.response_cache AND r.started_at>=now()-interval '30 days' AND ($1 OR k.project_id=ANY($2::uuid[])) AND ($3='' OR r.route_slug=$3) GROUP BY f.currency ORDER BY f.currency`, p.AllProjects, p.ProjectIDs(), route)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	costs := []map[string]string{}
	for rows.Next() {
		var currency, amount string
		if err = rows.Scan(&currency, &amount); err != nil {
			return access.Reply{}, err
		}
		costs = append(costs, map[string]string{"currency": currency, "amount": amount})
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	configured := s.Gateway != nil && s.Gateway.Cache != nil
	return access.OK(map[string]any{"configured": configured, "requests": requests, "hits": hits, "misses": misses, "bypasses": bypasses, "avoided_costs": costs}), nil
}
