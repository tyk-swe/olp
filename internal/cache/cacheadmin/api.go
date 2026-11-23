package cacheadmin

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
)

type API struct {
	server *access.Server
	store  *cache.Store
}

func NewAPI(server *access.Server, store *cache.Store) *API {
	return &API{server: server, store: store}
}

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "POST /api/v1/cache/purge", a.purge)
	a.server.Route(mux, "GET /api/v1/cache/statistics", a.statistics)
}

func (a *API) purge(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var in struct {
		RouteID   *string `json:"route_id"`
		ProjectID *string `json:"project_id"`
	}
	if err := httpkit.Decode(r, &in); err != nil {
		return httpkit.Reply{}, err
	}
	if (in.RouteID == nil) == (in.ProjectID == nil) {
		return httpkit.Reply{}, httpkit.Invalid("scope", "Supply exactly one route_id or project_id.")
	}
	if a.store == nil {
		return httpkit.Reply{}, httpkit.Fail(503, "cache_unavailable", "The response cache is unavailable.")
	}
	return access.Transact(a.server, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		kind, id := "project", in.ProjectID
		project := in.ProjectID
		if in.RouteID != nil {
			kind, id = "route", in.RouteID
		}
		parsed, err := uuid.Parse(*id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, httpkit.Invalid(kind+"_id", "Use a "+kind+" UUID.")
		}
		canonical := parsed.String()
		id = &canonical
		if kind == "route" {
			if err = tx.QueryRow(r.Context(), "SELECT project_id::text FROM olp.routes WHERE id=$1", *id).Scan(&project); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
		}
		if kind == "project" {
			project = id
			if err = a.server.RequireProject(r.Context(), tx, p, project); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
		}
		if err = p.Project(project, authz.Change); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = a.store.Purge(r.Context(), kind, *id); err != nil {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(503, "cache_unavailable", "The response cache is unavailable.")
		}
		return httpkit.OK(map[string]bool{"purged": true}), access.Event{Action: "cache.purge", Resource: kind, ID: *id}, nil
	})
}

func (a *API) statistics(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	route := r.URL.Query().Get("route")
	if route != "" && !routeidentity.ValidSlug(route) {
		return httpkit.Reply{}, httpkit.Invalid("route", "Use a route slug.")
	}
	var requests, hits, misses, bypasses int64
	const bounds = ` FROM olp.requests r JOIN olp.api_keys k ON k.id=r.api_key_id WHERE r.started_at>=now()-interval '30 days' AND ($1 OR k.project_id=ANY($2::uuid[])) AND ($3='' OR r.route_slug=$3)`
	err := a.server.Pool.QueryRow(r.Context(), `SELECT count(*) FILTER (WHERE r.cache_result IS NOT NULL),count(*) FILTER (WHERE r.cache_result IN ('hit','semantic-hit')),count(*) FILTER (WHERE r.cache_result='miss'),count(*) FILTER (WHERE r.cache_result='bypass')`+bounds, p.AllProjects, p.ProjectIDs(), route).Scan(&requests, &hits, &misses, &bypasses)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := a.server.Pool.Query(r.Context(), `SELECT COALESCE(f.currency,''),sum(f.cache_avoided_cost)::text FROM olp.attempt_usage_facts f JOIN olp.requests r ON r.id=f.request_id AND r.started_at=f.request_started_at JOIN olp.api_keys k ON k.id=r.api_key_id WHERE f.response_cache AND r.started_at>=now()-interval '30 days' AND ($1 OR k.project_id=ANY($2::uuid[])) AND ($3='' OR r.route_slug=$3) GROUP BY f.currency ORDER BY f.currency`, p.AllProjects, p.ProjectIDs(), route)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	costs := []map[string]string{}
	for rows.Next() {
		var currency, amount string
		if err = rows.Scan(&currency, &amount); err != nil {
			return httpkit.Reply{}, err
		}
		costs = append(costs, map[string]string{"currency": currency, "amount": amount})
	}
	if err = rows.Err(); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.OK(map[string]any{"configured": a.store != nil, "requests": requests, "hits": hits, "misses": misses, "bypasses": bypasses, "avoided_costs": costs}), nil
}
