// Package guardrails manages reusable, project-scoped definitions for the
// existing bounded content-policy engine. Route publication copies a selected
// immutable policy; serving never resolves management definitions per request.
package guardrails

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
)

type Server struct{ Access *access.Server }

type Definition struct {
	ID         string                `json:"id"`
	ProjectID  string                `json:"project_id"`
	Name       string                `json:"name"`
	Type       string                `json:"type"`
	ETag       string                `json:"etag"`
	RevisionID string                `json:"revision_id"`
	Revision   int64                 `json:"revision"`
	Policy     *contentpolicy.Policy `json:"policy"`
	RetiredAt  *time.Time            `json:"retired_at"`
}

type input struct {
	Name      string          `json:"name"`
	Type      string          `json:"type,omitempty"`
	ProjectID *string         `json:"project_id,omitempty"`
	Policy    json.RawMessage `json:"policy"`
}

func (s *Server) Register(mux *http.ServeMux) {
	a := s.Access
	a.Route(mux, "GET /api/v1/guardrails", s.list)
	a.Route(mux, "POST /api/v1/guardrails", s.create, access.MaxBody(128<<10))
	a.Route(mux, "GET /api/v1/guardrails/{guardrail_id}", s.get)
	a.Route(mux, "GET /api/v1/guardrails/{guardrail_id}/revisions/{revision_id}", s.revision)
	a.Route(mux, "PUT /api/v1/guardrails/{guardrail_id}", s.update, access.MaxBody(128<<10))
	a.Route(mux, "DELETE /api/v1/guardrails/{guardrail_id}", s.retire)
}

func (s *Server) revision(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "guardrail_id")
	if err != nil {
		return access.Reply{}, err
	}
	revisionID, err := access.IDParam(r, "revision_id")
	if err != nil {
		return access.Reply{}, err
	}
	current, err := load(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&current.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	var version int64
	var policy json.RawMessage
	var created time.Time
	if err = s.Access.Pool.QueryRow(r.Context(), "SELECT revision,policy,created_at FROM olp.guardrail_revisions WHERE guardrail_id=$1 AND id=$2", id, revisionID).Scan(&version, &policy, &created); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(map[string]any{"id": revisionID, "guardrail_id": id, "revision": version, "policy": policy, "created_at": created}, revisionID), nil
}

const columns = "g.id::text,g.project_id::text,g.name,g.type,g.etag::text,v.id::text,v.revision,v.policy,g.retired_at"
const joined = " FROM olp.guardrails g JOIN olp.guardrail_revisions v ON v.id=g.latest_revision_id"

func scan(row pgx.Row) (*Definition, error) {
	var value Definition
	var policy []byte
	if err := row.Scan(&value.ID, &value.ProjectID, &value.Name, &value.Type, &value.ETag, &value.RevisionID, &value.Revision, &policy, &value.RetiredAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(policy, &value.Policy); err != nil {
		return nil, err
	}
	return &value, nil
}

func load(ctx context.Context, q access.Queryer, id string, lock bool) (*Definition, error) {
	query := "SELECT " + columns + joined + " WHERE g.id=$1"
	if lock {
		query += " FOR UPDATE OF g"
	}
	return scan(q.QueryRow(ctx, query, id))
}

func (s *Server) list(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	if len(search) > 200 {
		return access.Reply{}, access.Invalid("search", "Use at most 200 bytes.")
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+columns+joined+" WHERE g.retired_at IS NULL AND g.id<$1 AND ($2 OR g.project_id=ANY($3::uuid[])) AND ($5='' OR g.name ILIKE '%'||$5||'%') ORDER BY g.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1, search)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, map[string]any{"id": value.ID, "project_id": value.ProjectID, "name": value.Name, "type": value.Type, "etag": value.ETag, "revision_id": value.RevisionID, "revision": value.Revision, "policy": value.Policy, "retired_at": value.RetiredAt})
	}
	return access.ListReply(items, page), rows.Err()
}

func (s *Server) get(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "guardrail_id")
	if err != nil {
		return access.Reply{}, err
	}
	value, err := load(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&value.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(value, value.ETag), nil
}

func validate(in *input) (*contentpolicy.Policy, error) {
	in.Name = strings.TrimSpace(in.Name)
	if !utf8.ValidString(in.Name) || utf8.RuneCountInString(in.Name) < 1 || utf8.RuneCountInString(in.Name) > 100 || strings.ContainsFunc(in.Name, unicode.IsControl) {
		return nil, access.Invalid("name", "Choose a name of 1–100 characters without control characters.")
	}
	raw := bytes.TrimSpace(in.Policy)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, access.Invalid("policy", "Supply a bounded content-policy object.")
	}
	policy, err := contentpolicy.Decode(in.Policy)
	if err != nil {
		// Parser diagnostics can include operator patterns; keep failures bounded
		// and content-free, like the engine's request evidence.
		return nil, access.Invalid("policy", "Supply valid bounded RE2 block or redact rules.")
	}
	return policy, nil
}

func (s *Server) create(r *http.Request, _ access.Principal) (access.Reply, error) {
	return s.write(r, "", false)
}
func (s *Server) update(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "guardrail_id")
	if err != nil {
		return access.Reply{}, err
	}
	return s.write(r, id, false)
}
func (s *Server) retire(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "guardrail_id")
	if err != nil {
		return access.Reply{}, err
	}
	return s.write(r, id, true)
}

func (s *Server) write(r *http.Request, id string, retire bool) (access.Reply, error) {
	var in input
	var policy *contentpolicy.Policy
	if !retire {
		if err := access.DecodeUnique(r, &in, 128<<10); err != nil {
			return access.Reply{}, err
		}
		var err error
		policy, err = validate(&in)
		if err != nil {
			return access.Reply{}, err
		}
	}
	a := s.Access
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	var current *Definition
	if id != "" {
		current, err = load(r.Context(), tx, id, true)
		if err != nil {
			return access.Reply{}, err
		}
		if err = p.Project(&current.ProjectID, access.Change); err != nil {
			return access.Reply{}, err
		}
	} else {
		if in.Type != "builtin.regex" {
			return access.Reply{}, access.Invalid("type", "Use builtin.regex.")
		}
		if in.ProjectID == nil {
			return access.Reply{}, access.Invalid("project_id", "Choose the owning project.")
		}
		if err = a.RequireProject(r.Context(), tx, p, in.ProjectID); err != nil {
			return access.Reply{}, err
		}
	}
	claim, replayed, err := a.Replay(r, tx, p, in)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	if current != nil {
		if err = access.Match(r, current.ETag); err != nil {
			return access.Reply{}, err
		}
		if current.RetiredAt != nil {
			return access.Reply{}, access.Fail(409, "guardrail_retired", "Create a new guardrail rather than changing retained history.")
		}
		if in.ProjectID != nil || in.Type != "" {
			return access.Reply{}, access.Invalid("project_id", "A guardrail's project and type are immutable.")
		}
	}
	etag := access.NewID()
	if retire {
		_, err = tx.Exec(r.Context(), "UPDATE olp.guardrails SET retired_at=now(),etag=$2 WHERE id=$1", id, etag)
	} else {
		next := &Definition{Name: in.Name, Policy: policy}
		if current == nil {
			next.ProjectID, next.Type = *in.ProjectID, in.Type
		} else {
			next.ProjectID, next.Type = current.ProjectID, current.Type
		}
		err = Save(r.Context(), tx, p.UserID(), current, next)
		id, etag = next.ID, next.ETag
	}
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return access.Reply{}, access.Fail(409, "guardrail_exists", "Choose an unused guardrail name in this project.")
		}
		return access.Reply{}, err
	}
	action := "guardrail.update"
	if current == nil {
		action = "guardrail.create"
	}
	if retire {
		action = "guardrail.retire"
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), action, "guardrail", id, "success"); err != nil {
		return access.Reply{}, err
	}
	reply := access.Reply{Status: http.StatusNoContent}
	if !retire {
		value, err := load(r.Context(), tx, id, false)
		if err != nil {
			return access.Reply{}, err
		}
		reply = access.Detail(value, etag)
		if current == nil {
			reply.Status, reply.Location = http.StatusCreated, "/api/v1/guardrails/"+id
		}
	}
	if err = a.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}
