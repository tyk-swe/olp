package agents

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/egress"
)

type Server struct {
	Access        *access.Server
	Egress        *egress.Policy
	ValidateAgent func(context.Context, agentcontract.Definition) error
}

func (s *Server) Register(mux *http.ServeMux) {
	for _, kind := range []string{"toolset", "agent", "prompt"} {
		root := "/api/v1/" + kind + "s"
		s.Access.Route(mux, "GET "+root, func(r *http.Request, p access.Principal) (access.Reply, error) { return s.list(r, p, kind) })
		s.Access.Route(mux, "POST "+root, func(r *http.Request, p access.Principal) (access.Reply, error) { return s.write(r, kind, "create") }, access.MaxBody(1<<20))
		s.Access.Route(mux, "GET "+root+"/{publication_id}", func(r *http.Request, p access.Principal) (access.Reply, error) { return s.get(r, p, kind) })
		s.Access.Route(mux, "PUT "+root+"/{publication_id}", func(r *http.Request, p access.Principal) (access.Reply, error) { return s.write(r, kind, "update") }, access.MaxBody(1<<20))
		s.Access.Route(mux, "DELETE "+root+"/{publication_id}", func(r *http.Request, p access.Principal) (access.Reply, error) { return s.write(r, kind, "retire") })
		s.Access.Route(mux, "POST "+root+"/{publication_id}/publish", func(r *http.Request, p access.Principal) (access.Reply, error) { return s.write(r, kind, "publish") })
		s.Access.Route(mux, "GET "+root+"/{publication_id}/revisions/{revision_id}", func(r *http.Request, p access.Principal) (access.Reply, error) { return s.revision(r, p, kind) })
	}
	s.Access.Route(mux, "PUT /api/v1/prompts/{publication_id}/labels/{label}", func(r *http.Request, p access.Principal) (access.Reply, error) { return s.write(r, "prompt", "label") })
}

func (s *Server) list(r *http.Request, p access.Principal, kind string) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT id::text,kind,project_id::text,name,etag::text,latest_revision_id::text,retired FROM olp.agent_publications WHERE kind=$1 AND NOT retired AND id<$2 AND ($3 OR project_id=ANY($4::uuid[])) ORDER BY id DESC LIMIT $5`, kind, page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, kind, project, name, etag string
		var revision *string
		var retired bool
		if err = rows.Scan(&id, &kind, &project, &name, &etag, &revision, &retired); err != nil {
			return access.Reply{}, err
		}
		items = append(items, map[string]any{"id": id, "kind": kind, "project_id": project, "name": name, "etag": etag, "revision_id": revision, "retired": retired})
	}
	return access.ListReply(items, page), rows.Err()
}

func (s *Server) visible(r *http.Request, p access.Principal, kind string) (*Publication, error) {
	id, err := access.IDParam(r, "publication_id")
	if err != nil {
		return nil, err
	}
	d, err := Load(r.Context(), s.Access.Pool, id, kind, false)
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
	v, err := LoadRevision(r.Context(), s.Access.Pool, d.ID, id)
	if err != nil {
		return access.Reply{}, mapConflict(err)
	}
	// The definition is immutable; current/label metadata follows its parent.
	return access.Detail(v, d.ETag), nil
}

func (s *Server) write(r *http.Request, kind, action string) (access.Reply, error) {
	var request struct {
		Draft
		Credential        json.RawMessage `json:"credential,omitempty"`
		EntraClientSecret *string         `json:"entra_client_secret,omitempty"`
	}
	var draft Draft
	var label struct {
		RevisionID string `json:"revision_id"`
	}
	var input any = struct{}{}
	if action == "create" || action == "update" {
		if err := access.DecodeUnique(r, &request, 1<<20); err != nil {
			return access.Reply{}, err
		}
		draft = request.Draft
		input = request
		if err := draft.Validate(kind); err != nil {
			return access.Reply{}, access.Invalid("draft", err.Error())
		}
	}
	if action == "label" {
		if err := access.DecodeUnique(r, &label, 1024); err != nil {
			return access.Reply{}, err
		}
		input = label
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
	var current *Publication
	if action != "create" {
		id, err := access.IDParam(r, "publication_id")
		if err != nil {
			return access.Reply{}, err
		}
		current, err = Load(r.Context(), tx, id, kind, true)
		if err != nil {
			return access.Reply{}, mapConflict(err)
		}
		if err = p.Project(&current.ProjectID, access.Change); err != nil {
			return access.Reply{}, err
		}
	} else if err = s.Access.RequireProject(r.Context(), tx, p, &draft.ProjectID); err != nil {
		return access.Reply{}, err
	}
	claim, replay, err := s.Access.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replay != nil {
		return access.Commit(r, tx, *replay)
	}
	if current != nil {
		if err = access.Match(r, current.ETag); err != nil {
			return access.Reply{}, err
		}
		if current.Retired {
			return access.Reply{}, access.Fail(409, "publication_retired", "Retired history cannot be changed.")
		}
	}
	next := current
	switch action {
	case "create", "update":
		credentialID, credentialErr := s.credential(r.Context(), tx, current, draft, request.Credential, request.EntraClientSecret)
		if credentialErr != nil {
			return access.Reply{}, credentialErr
		}
		credentialDraft := draftWithCredential(draft, credentialID)
		if err = AuthorizeTrustChange(p, current, credentialDraft); err != nil {
			return access.Reply{}, err
		}
		next = &Publication{Kind: kind, Draft: credentialDraft}
		err = SaveDraft(r.Context(), tx, p.UserID(), current, next, s.Egress)
	case "publish":
		err = Publish(r.Context(), tx, p.UserID(), current)
		if err == nil && current.Kind == "agent" && (s.ValidateAgent == nil || s.ValidateAgent(r.Context(), *current.Draft.Agent) != nil) {
			return access.Reply{}, access.Invalid("agent", "The pinned agent card must declare the configured protocol and JSON-RPC endpoint.")
		}
	case "retire":
		next.ETag = access.NewID()
		next.Retired = true
		_, err = tx.Exec(r.Context(), `UPDATE olp.agent_publications SET retired=true,etag=$2 WHERE id=$1`, next.ID, next.ETag)
	case "label":
		next.ETag = access.NewID()
		err = SetLabel(r.Context(), tx, next, r.PathValue("label"), label.RevisionID, next.ETag)
		if err == nil {
			_, err = tx.Exec(r.Context(), `UPDATE olp.agent_publications SET etag=$2 WHERE id=$1`, next.ID, next.ETag)
		}
	}
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			err = access.Fail(409, "publication_exists", "Choose an unused name in this project.")
		}
		return access.Reply{}, err
	}
	if action == "publish" || action == "retire" || action == "label" {
		if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
			return access.Reply{}, err
		}
		if s.Access.RuntimePublisher != nil {
			if err = s.Access.RuntimePublisher(r.Context(), tx, p.UserID()); err != nil {
				return access.Reply{}, err
			}
		}
	}
	if err = access.AuditForProject(r.Context(), tx, r, p.Actor(), kind+"."+action, kind, next.ID, "success", &next.ProjectID); err != nil {
		return access.Reply{}, err
	}
	reply := access.Detail(next, next.ETag)
	if action == "create" {
		reply.Status = 201
		reply.Location = "/api/v1/" + kind + "s/" + next.ID
	}
	if action == "retire" {
		reply = access.Reply{Status: 204}
	}
	if err = s.Access.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}

// No caller-selected egress or effectful consumer methods are registered here.
