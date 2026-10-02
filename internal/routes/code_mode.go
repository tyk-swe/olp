package routes

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/runtime"
)

func (s *Server) registerCodeMode(mux *http.ServeMux) {
	s.Access.Route(mux, "GET /api/v1/code/routes", s.codeRoutes)
	s.Access.Route(mux, "POST /api/v1/code/routes", s.writeCodeRoute)
	s.Access.Route(mux, "PUT /api/v1/code/routes/{id}", s.writeCodeRoute)
	s.Access.Route(mux, "POST /api/v1/code/routes/{id}/publish", s.publishCodeRoute)
	s.Access.Route(mux, "GET /api/v1/code/routes/{id}/revisions", s.codeRevisions)
}

func (s *Server) codeRoutes(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.Access.CodeList(r, p, `SELECT x.draft||jsonb_build_object('etag',x.etag) FROM olp.code_routes x`)
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
	if !access.RouteSlug.MatchString(in.Slug) {
		return access.Reply{}, access.Invalid("slug", "Use a route slug.")
	}
	if _, err := access.ParseUUID(in.PoolID); err != nil {
		return access.Reply{}, err
	}
	if err := codemode.ValidateModels(in.Models); err != nil {
		return access.Reply{}, access.Invalid("models", err.Error())
	}
	return s.Access.CodeWrite(r, "code_routes", "code_route", in.ProjectID, in, func(tx pgx.Tx, p access.Principal, id, etag string) (any, error) {
		var matches bool
		if err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM olp.code_pools WHERE id=$1 AND project_id=$2)`, in.PoolID, in.ProjectID).Scan(&matches); err != nil {
			return nil, err
		}
		if !matches {
			return nil, access.Fail(404, "not_found", "The pool was not found.")
		}
		if r.PathValue("id") != "" {
			var slug string
			if err := tx.QueryRow(r.Context(), `SELECT slug FROM olp.code_routes WHERE id=$1`, id).Scan(&slug); err != nil {
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
		out := codemode.Route{ID: id, ProjectID: in.ProjectID, Slug: in.Slug, PoolID: in.PoolID, Models: in.Models, Enabled: in.Enabled, ETag: etag}
		document, _ := json.Marshal(out)
		_, err := tx.Exec(r.Context(), `INSERT INTO olp.code_routes(id,project_id,slug,draft,etag,created_by) VALUES($1,$2,$3,$4,$5,$6)
			ON CONFLICT(id) DO UPDATE SET draft=excluded.draft,etag=excluded.etag`, id, in.ProjectID, in.Slug, document, etag, p.UserID())
		return out, err
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
	route.RevisionID = access.NewID()
	route.ETag = access.NewID()
	now := time.Now().UTC()
	route.PublishedAt = &now
	if err = tx.QueryRow(r.Context(), `SELECT COALESCE(max(revision),0)+1 FROM olp.code_route_revisions WHERE route_id=$1`, id).Scan(&route.Revision); err != nil {
		return access.Reply{}, err
	}
	document, _ = json.Marshal(route)
	if _, err = tx.Exec(r.Context(), `INSERT INTO olp.code_route_revisions(id,route_id,revision,document,created_by,published_at) VALUES($1,$2,$3,$4,$5,$6)`, route.RevisionID, id, route.Revision, document, p.UserID(), now); err != nil {
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
	out := access.Detail(route, route.ETag)
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
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT jsonb_build_object('id',id,'route',document) FROM olp.code_route_revisions WHERE route_id=$1 AND id<$2 ORDER BY id DESC LIMIT $3`, id, page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	return access.ListReply(items, page), err
}
