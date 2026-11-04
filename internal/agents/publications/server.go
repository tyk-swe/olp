package publications

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/egress"
)

type Server struct {
	access        *access.Server
	egress        *egress.Policy
	validateAgent func(context.Context, agentcontract.Definition) error
}

func (s *Server) Register(mux *http.ServeMux) {
	for _, kind := range []string{"toolset", "agent", "prompt"} {
		root := "/api/v1/" + kind + "s"
		s.access.Route(mux, "GET "+root, func(r *http.Request, p access.Principal) (access.Reply, error) { return s.list(r, p, kind) })
		s.access.Route(mux, "POST "+root, func(r *http.Request, p access.Principal) (access.Reply, error) { return s.write(r, kind, "create") }, access.MaxBody(1<<20))
		s.access.Route(mux, "GET "+root+"/{publication_id}", func(r *http.Request, p access.Principal) (access.Reply, error) { return s.get(r, p, kind) })
		s.access.Route(mux, "PUT "+root+"/{publication_id}", func(r *http.Request, p access.Principal) (access.Reply, error) { return s.write(r, kind, "update") }, access.MaxBody(1<<20))
		s.access.Route(mux, "DELETE "+root+"/{publication_id}", func(r *http.Request, p access.Principal) (access.Reply, error) { return s.write(r, kind, "retire") })
		s.access.Route(mux, "POST "+root+"/{publication_id}/publish", func(r *http.Request, p access.Principal) (access.Reply, error) { return s.write(r, kind, "publish") })
		s.access.Route(mux, "GET "+root+"/{publication_id}/revisions/{revision_id}", func(r *http.Request, p access.Principal) (access.Reply, error) { return s.revision(r, p, kind) })
	}
	s.access.Route(mux, "PUT /api/v1/prompts/{publication_id}/labels/{label}", func(r *http.Request, p access.Principal) (access.Reply, error) { return s.write(r, "prompt", "label") })
}

func (s *Server) list(r *http.Request, p access.Principal, kind string) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(), `SELECT id::text,kind,project_id::text,name,etag::text,latest_revision_id::text,retired FROM olp.agent_publications WHERE kind=$1 AND NOT retired AND id<$2 AND ($3 OR project_id=ANY($4::uuid[])) ORDER BY id DESC LIMIT $5`, kind, page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items, collectErr := pgx.CollectRows(rows, func(row pgx.CollectableRow) (publicationSummary, error) {
		var item publicationSummary
		err := row.Scan(&item.ID, &item.Kind, &item.ProjectID, &item.Name, &item.ETag, &item.RevisionID, &item.Retired)
		return item, err
	})
	if collectErr != nil {
		return access.Reply{}, collectErr
	}

	return access.ListReplyBy(items, page, func(item publicationSummary) string { return item.ID }), rows.Err()
}

func (s *Server) visible(r *http.Request, p access.Principal, kind string) (*agentcontract.Publication, error) {
	id, err := access.IDParam(r, "publication_id")
	if err != nil {
		return nil, err
	}
	d, err := Load(r.Context(), s.access.Pool, id, kind, false)
	if err != nil {
		return nil, mapConflict(err)
	}
	if err = p.Project(&d.ProjectID, access.View); err != nil {
		return nil, err
	}
	return d, nil
}
func (s *Server) get(r *http.Request, p access.Principal, kind string) (access.Reply, error) {
	d, err := s.visible(r, p, kind)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(d, d.ETag), nil
}
func (s *Server) revision(r *http.Request, p access.Principal, kind string) (access.Reply, error) {
	d, err := s.visible(r, p, kind)
	if err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "revision_id")
	if err != nil {
		return access.Reply{}, err
	}
	v, err := LoadRevision(r.Context(), s.access.Pool, d.ID, id)
	if err != nil {
		return access.Reply{}, mapConflict(err)
	}
	return access.Detail(v, d.ETag), nil
}

type publicationSummary struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	ProjectID  string  `json:"project_id"`
	Name       string  `json:"name"`
	ETag       string  `json:"etag"`
	RevisionID *string `json:"revision_id"`
	Retired    bool    `json:"retired"`
}

func New(a *access.Server, policy *egress.Policy, validate func(context.Context, agentcontract.Definition) error) *Server {
	return &Server{access: a, egress: policy, validateAgent: validate}
}
