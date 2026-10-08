package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/runtime"
)

func (s *Server) registerCodeMode(mux *http.ServeMux) {
	s.Access.Route(mux, "GET /api/v1/code/routes", s.codeRoutes)
	s.Access.Route(mux, "POST /api/v1/code/routes", s.writeCodeRoute)
	s.Access.Route(mux, "PUT /api/v1/code/routes/{id}", s.writeCodeRoute)
	s.Access.Route(mux, "POST /api/v1/code/routes/{id}/publish", s.publishCodeRoute)
	s.Access.Route(mux, "GET /api/v1/code/routes/{id}/revisions", s.codeRevisions)
	s.Access.Route(mux, "GET /api/v1/code/routes/{id}/client-config", s.CodeClientConfiguration)
}

// codeRoute is a route as management returns it: with the adapters its latest
// published revision serves, derived from the connections it froze.
type codeRoute struct {
	codemode.Route
	Adapters []codemode.Adapter `json:"adapters"`
}

func (s *Server) codeRoutes(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.Access.CodeList(r, p, `SELECT x.draft||jsonb_build_object('etag',x.etag,
		'revision_id',coalesce(v.id::text,''),'revision',coalesce(v.revision,0),'published_at',v.published_at)
		||jsonb_build_object('adapters',`+codeadapter.SQLAdapters("v.connections")+`)
		FROM olp.code_routes x LEFT JOIN olp.code_route_revisions v ON v.id=x.latest_revision_id`)
}

type codeRouteInput struct {
	ProjectID string   `json:"project_id"`
	Slug      string   `json:"slug"`
	PoolID    string   `json:"pool_id"`
	Models    []string `json:"models"`
	Enabled   bool     `json:"enabled"`
}

func (s *Server) writeCodeRoute(r *http.Request, _ access.Principal) (access.Reply, error) {
	var in codeRouteInput
	if err := access.Decode(r, &in); err != nil {
		return access.Reply{}, err
	}
	projectID, err := access.ParseUUID(in.ProjectID)
	if err != nil {
		return access.Reply{}, err
	}
	in.ProjectID = projectID
	if !access.RouteSlug.MatchString(in.Slug) {
		return access.Reply{}, access.Invalid("slug", "Use a route slug.")
	}
	if in.PoolID, err = access.ParseUUID(in.PoolID); err != nil {
		return access.Reply{}, err
	}
	if err := codemode.ValidateModels(in.Models); err != nil {
		return access.Reply{}, access.Invalid("models", err.Error())
	}
	return s.Access.CodeWrite(r, "code_routes", "code_route", in.ProjectID, in, func(tx pgx.Tx, p access.Principal, id, etag string) (any, error) {
		out := codemode.Route{ID: id, ProjectID: in.ProjectID, Slug: in.Slug, PoolID: in.PoolID, Models: in.Models, Enabled: in.Enabled, ETag: etag}
		adapters := []codemode.Adapter{}
		var matches bool
		if err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM olp.code_pools WHERE id=$1 AND project_id=$2)`, in.PoolID, in.ProjectID).Scan(&matches); err != nil {
			return nil, err
		}
		if !matches {
			return nil, access.Fail(404, "not_found", "The pool was not found.")
		}
		if r.PathValue("id") != "" {
			var slug string
			if err := tx.QueryRow(r.Context(), `SELECT x.slug,coalesce(v.id::text,''),coalesce(v.revision,0),v.published_at,`+codeadapter.SQLAdapters("v.connections")+`
				FROM olp.code_routes x LEFT JOIN olp.code_route_revisions v ON v.id=x.latest_revision_id WHERE x.id=$1`, id).Scan(&slug, &out.RevisionID, &out.Revision, &out.PublishedAt, &adapters); err != nil {
				return nil, err
			}
			if slug != in.Slug {
				return nil, access.Invalid("slug", "The route slug is immutable.")
			}
		}
		if err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM olp.routes WHERE slug=$1) OR EXISTS(SELECT 1 FROM olp.route_drafts WHERE slug=$1)`, in.Slug).Scan(&matches); err != nil {
			return nil, err
		}
		if matches {
			return nil, access.Invalid("slug", "This slug belongs to an ordinary route.")
		}
		document, _ := json.Marshal(out)
		_, err := tx.Exec(r.Context(), `INSERT INTO olp.code_routes(id,project_id,slug,draft,etag,created_by) VALUES($1,$2,$3,$4,$5,$6)
			ON CONFLICT(id) DO UPDATE SET draft=excluded.draft,etag=excluded.etag`, id, in.ProjectID, in.Slug, document, etag, p.UserID())
		return codeRoute{Route: out, Adapters: adapters}, err
	})
}

func (s *Server) publishCodeRoute(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "id")
	if err != nil {
		return access.Reply{}, err
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
	var document []byte
	var project, etag string
	if err = tx.QueryRow(r.Context(), `SELECT draft,project_id::text,etag::text FROM olp.code_routes WHERE id=$1`, id).Scan(&document, &project, &etag); err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&project, access.Change); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, p, id)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	if err = access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	var route codemode.Route
	if err = json.Unmarshal(document, &route); err != nil {
		return access.Reply{}, err
	}
	if route.Enabled {
		for _, model := range route.Models {
			var available bool
			err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM olp.code_pool_accounts pa JOIN olp.code_accounts a ON a.id=pa.account_id JOIN olp.provider_credentials c ON c.id=a.credential_id JOIN olp.provider_grants g ON g.credential_id=c.id
				WHERE pa.pool_id=$1 AND a.enabled AND a.project_id=$2 AND a.models ? $3 AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now()) AND c.principal=a.principal)`, route.PoolID, project, model).Scan(&available)
			if err != nil {
				return access.Reply{}, err
			}
			if !available {
				return access.Reply{}, access.Invalid("models", "Every model needs an enrolled pool account.")
			}
		}
	}
	connections, adapters, err := codePublication(r.Context(), tx, route.PoolID)
	if err != nil {
		return access.Reply{}, err
	}
	route.RevisionID = access.NewID()
	route.ETag = access.NewID()
	now := time.Now().UTC().Truncate(time.Microsecond)
	route.PublishedAt = &now
	if err = tx.QueryRow(r.Context(), `SELECT COALESCE(max(revision),0)+1 FROM olp.code_route_revisions WHERE route_id=$1`, id).Scan(&route.Revision); err != nil {
		return access.Reply{}, err
	}
	document, _ = json.Marshal(route)
	if _, err = tx.Exec(r.Context(), `INSERT INTO olp.code_route_revisions(id,route_id,revision,document,created_by,published_at,connections)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, route.RevisionID, id, route.Revision, document, p.UserID(), now, connections); err != nil {
		return access.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), `UPDATE olp.code_routes SET latest_revision_id=$2,etag=$3,draft=$4 WHERE id=$1`, id, route.RevisionID, route.ETag, document); err != nil {
		return access.Reply{}, err
	}
	if _, err = runtime.Publish(r.Context(), tx, p.UserID()); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "code_route.publish", "code_route", id, "success"); err != nil {
		return access.Reply{}, err
	}
	out := access.Detail(codeRoute{Route: route, Adapters: adapters}, route.ETag)
	if err = s.Access.CompleteReplay(r, tx, claim, out); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, out)
}

func (s *Server) codeRevisions(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "id")
	if err != nil {
		return access.Reply{}, err
	}
	var project string
	if err = s.Access.Pool.QueryRow(r.Context(), `SELECT project_id::text FROM olp.code_routes WHERE id=$1`, id).Scan(&project); err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&project, access.View); err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT jsonb_build_object('id',id,'route',document||jsonb_build_object('adapters',`+codeadapter.SQLAdapters("connections")+`))
		FROM olp.code_route_revisions WHERE route_id=$1 AND id<$2 ORDER BY id DESC LIMIT $3`, id, page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	return access.ListReply(items, page), err
}

// codePublication reads the configurations of the providers behind a pool's
// accounts, which a revision freezes as its connections, and the adapters
// they serve in table order. A pool may mix adapters.
func codePublication(ctx context.Context, tx pgx.Tx, poolID string) ([]byte, []codemode.Adapter, error) {
	rows, err := tx.Query(ctx, `SELECT p.id::text,p.configuration FROM olp.providers p
		WHERE EXISTS(SELECT 1 FROM olp.code_accounts a JOIN olp.code_pool_accounts pa ON pa.account_id=a.id WHERE pa.pool_id=$1 AND a.provider_id=p.id)`, poolID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	connections := map[string]json.RawMessage{}
	adapters := []codemode.Adapter{}
	for rows.Next() {
		var id string
		var configuration json.RawMessage
		if err = rows.Scan(&id, &configuration); err != nil {
			return nil, nil, err
		}
		var c struct {
			Kind      string `json:"kind"`
			AuthMode  string `json:"auth_mode"`
			ProfileID string `json:"profile_id"`
		}
		if err = json.Unmarshal(configuration, &c); err != nil {
			return nil, nil, err
		}
		connections[id] = configuration
		if v, ok := codeadapter.ForConnection(c.Kind, c.AuthMode, c.ProfileID); ok && !slices.Contains(adapters, v.Adapter) {
			adapters = append(adapters, v.Adapter)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	slices.SortFunc(adapters, codeadapter.Compare)
	encoded, err := json.Marshal(connections)
	return encoded, adapters, err
}
