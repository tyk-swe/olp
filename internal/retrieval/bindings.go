package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
)

type Input struct {
	ProjectID string   `json:"project_id"`
	Name      string   `json:"name"`
	Route     string   `json:"route"`
	TargetID  string   `json:"target_id"`
	Backend   string   `json:"backend"`
	Resource  string   `json:"backend_resource"`
	Fields    Fields   `json:"fields"`
	KeyGrants []string `json:"key_grants"`
	Enabled   bool     `json:"enabled"`
}
type Binding struct {
	Input
	ProviderID string     `json:"provider_id"`
	ID         string     `json:"id"`
	RevisionID string     `json:"revision_id"`
	Revision   int64      `json:"revision"`
	ETag       string     `json:"etag"`
	RetiredAt  *time.Time `json:"retired_at"`
}
type Server struct{ Access *access.Server }

func Validate(in *Input) error {
	in.Name = strings.TrimSpace(in.Name)
	in.Route = strings.TrimSpace(in.Route)
	if err := access.ValidText("name", in.Name, 100); err != nil {
		return err
	}
	if _, err := uuid.Parse(in.ProjectID); err != nil {
		return access.Invalid("project_id", "Choose an owning project.")
	}
	if _, err := uuid.Parse(in.TargetID); err != nil {
		return access.Invalid("target_id", "Choose one route target.")
	}
	if in.Route == "" || len(in.Route) > 128 || !ResourceValid(in.Backend, in.Resource) {
		return access.Invalid("backend_resource", "Choose a supported backend, route and native resource.")
	}
	if err := ValidateFields(in.Backend, in.Fields); err != nil {
		return access.Invalid("fields", err.Error())
	}
	if len(in.KeyGrants) > 2000 {
		return access.Invalid("key_grants", "Use at most 2000 explicit API-key grants.")
	}
	if in.KeyGrants == nil {
		in.KeyGrants = []string{}
	}
	for _, key := range in.KeyGrants {
		if parsed, err := uuid.Parse(key); err != nil || parsed.String() != key {
			return access.Invalid("key_grants", "Use canonical API-key UUIDs.")
		}
	}
	slices.Sort(in.KeyGrants)
	in.KeyGrants = slices.Compact(in.KeyGrants)
	return nil
}

const bindingColumns = "b.id::text,b.project_id::text,b.name,b.etag::text,b.retired_at,v.id::text,v.revision,v.definition"
const bindingJoin = " FROM olp.retrieval_bindings b JOIN olp.retrieval_binding_revisions v ON v.id=b.latest_revision_id"

func scanBinding(row pgx.Row) (*Binding, error) {
	d := &Binding{}
	var raw []byte
	var project, name string
	err := row.Scan(&d.ID, &project, &name, &d.ETag, &d.RetiredAt, &d.RevisionID, &d.Revision, &raw)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, d); err != nil {
		return nil, err
	}
	d.ProjectID, d.Name = project, name
	return d, nil
}
func Load(ctx context.Context, q access.Queryer, id string, lock bool) (*Binding, error) {
	query := "SELECT " + bindingColumns + bindingJoin + " WHERE b.id=$1"
	if lock {
		query += " FOR UPDATE OF b"
	}
	return scanBinding(q.QueryRow(ctx, query, id))
}
func Find(ctx context.Context, q access.Queryer, project, name string) (*Binding, error) {
	return scanBinding(q.QueryRow(ctx, "SELECT "+bindingColumns+bindingJoin+" WHERE b.project_id=$1 AND lower(b.name)=lower($2) AND b.retired_at IS NULL", project, name))
}
func List(ctx context.Context, q access.Queryer) ([]Binding, error) {
	rows, err := q.Query(ctx, "SELECT "+bindingColumns+bindingJoin+" WHERE b.retired_at IS NULL ORDER BY b.project_id,lower(b.name)")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Binding{}
	for rows.Next() {
		d, err := scanBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// ValidateAuthority runs in the management transaction. The serving path also
// verifies current published route, target, credential and API-key authority.
func ValidateAuthority(ctx context.Context, q access.Queryer, in Input) (string, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT p.id::text FROM (
 SELECT r.slug,r.project_id,v.targets,v.operations FROM olp.routes r JOIN olp.route_revisions v ON v.id=r.latest_revision_id WHERE r.state='active'
 UNION ALL SELECT d.slug,d.project_id,d.targets,d.operations FROM olp.route_drafts d WHERE d.state='draft'
 ) r CROSS JOIN LATERAL jsonb_array_elements(r.targets) t JOIN olp.providers p ON p.id=(t->>'provider_id')::uuid
 WHERE r.slug=$1 AND r.project_id=$2 AND t->>'id'=$3 AND p.kind=$4 AND t->>'provider_model'=$5
 AND (p.project_id IS NULL OR p.project_id=$2) AND r.operations ? 'retrieval'`, in.Route, in.ProjectID, in.TargetID, ProviderKind(in.Backend), in.Resource)
	if err != nil {
		return "", err
	}
	var providers []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return "", err
		}
		providers = append(providers, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", err
	}
	if len(providers) != 1 {
		return "", access.Invalid("target_id", "Choose a retrieval target in this project's route or draft with the matching backend kind and resource.")
	}
	var count int
	err = q.QueryRow(ctx, "SELECT count(*) FROM olp.api_keys WHERE id=ANY($1::uuid[]) AND project_id=$2 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now())", in.KeyGrants, in.ProjectID).Scan(&count)
	if err != nil {
		return "", err
	}
	if count != len(in.KeyGrants) {
		return "", access.Invalid("key_grants", "Every grant must name an active API key in this project.")
	}
	return providers[0], nil
}
func Save(ctx context.Context, tx pgx.Tx, actor string, current, next *Binding) error {
	if err := Validate(&next.Input); err != nil {
		return err
	}
	provider, err := ValidateAuthority(ctx, tx, next.Input)
	if err != nil {
		return err
	}
	next.ProviderID = provider
	next.ID, next.RevisionID, next.ETag, next.Revision = access.NewID(), access.NewID(), access.NewID(), 1
	if current != nil {
		if current.RetiredAt != nil || current.ProjectID != next.ProjectID {
			return access.Invalid("project_id", "The binding project is immutable and retired bindings cannot change.")
		}
		next.ID, next.Revision = current.ID, current.Revision+1
	}
	raw, err := json.Marshal(struct {
		Input
		ProviderID string `json:"provider_id"`
	}{next.Input, next.ProviderID})
	if err != nil {
		return err
	}
	if current == nil {
		_, err = tx.Exec(ctx, "INSERT INTO olp.retrieval_bindings(id,project_id,name,latest_revision_id,etag,created_by) VALUES($1,$2,$3,$4,$5,$6)", next.ID, next.ProjectID, next.Name, next.RevisionID, next.ETag, actor)
	} else {
		_, err = tx.Exec(ctx, "UPDATE olp.retrieval_bindings SET name=$2,latest_revision_id=$3,etag=$4 WHERE id=$1", next.ID, next.Name, next.RevisionID, next.ETag)
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO olp.retrieval_binding_revisions(id,binding_id,revision,definition,created_by) VALUES($1,$2,$3,$4,$5)", next.RevisionID, next.ID, next.Revision, raw, actor)
	return err
}
func (s *Server) Register(mux *http.ServeMux) {
	s.Access.Route(mux, "GET /api/v1/retrieval-bindings", s.list)
	s.Access.Route(mux, "POST /api/v1/retrieval-bindings", s.write)
	s.Access.Route(mux, "GET /api/v1/retrieval-bindings/{binding_id}", s.get)
	s.Access.Route(mux, "PUT /api/v1/retrieval-bindings/{binding_id}", s.write)
	s.Access.Route(mux, "DELETE /api/v1/retrieval-bindings/{binding_id}", s.write)
	s.Access.Route(mux, "GET /api/v1/retrieval-bindings/{binding_id}/revisions/{revision_id}", s.revision)
}
func (s *Server) list(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+bindingColumns+bindingJoin+" WHERE b.retired_at IS NULL AND b.id<$1 AND ($2 OR b.project_id=ANY($3::uuid[])) ORDER BY b.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		d, err := scanBinding(rows)
		if err != nil {
			return access.Reply{}, err
		}
		raw, _ := json.Marshal(d)
		var item map[string]any
		_ = json.Unmarshal(raw, &item)
		items = append(items, item)
	}
	return access.ListReply(items, page), rows.Err()
}
func (s *Server) get(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "binding_id")
	if err != nil {
		return access.Reply{}, err
	}
	d, err := Load(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&d.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(d, d.ETag), nil
}
func (s *Server) revision(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "binding_id")
	if err != nil {
		return access.Reply{}, err
	}
	version, err := access.IDParam(r, "revision_id")
	if err != nil {
		return access.Reply{}, err
	}
	d, err := Load(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&d.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	var raw json.RawMessage
	var n int64
	err = s.Access.Pool.QueryRow(r.Context(), "SELECT definition,revision FROM olp.retrieval_binding_revisions WHERE id=$1 AND binding_id=$2", version, id).Scan(&raw, &n)
	if err != nil {
		return access.Reply{}, err
	}
	var definition Input
	if err = json.Unmarshal(raw, &definition); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(map[string]any{"id": version, "binding_id": id, "revision": n, "definition": definition}, version), nil
}
func (s *Server) write(r *http.Request, _ access.Principal) (access.Reply, error) {
	retire := r.Method == http.MethodDelete
	var in Input
	if !retire {
		if err := access.DecodeUnique(r, &in, 64<<10); err != nil {
			return access.Reply{}, err
		}
		if err := Validate(&in); err != nil {
			return access.Reply{}, err
		}
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
	if err = p.Authorize(access.Configure); err != nil {
		return access.Reply{}, err
	}
	id := r.PathValue("binding_id")
	var current *Binding
	if id != "" {
		if _, err = access.IDParam(r, "binding_id"); err != nil {
			return access.Reply{}, err
		}
		current, err = Load(r.Context(), tx, id, true)
		if err != nil {
			return access.Reply{}, err
		}
		if err = p.Project(&current.ProjectID, access.Change); err != nil {
			return access.Reply{}, err
		}
	} else {
		if err = s.Access.RequireProject(r.Context(), tx, p, &in.ProjectID); err != nil {
			return access.Reply{}, err
		}
	}
	claim, replay, err := s.Access.Replay(r, tx, p, in)
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
		if current.RetiredAt != nil {
			return access.Reply{}, access.Fail(409, "retrieval_binding_retired", "Create a new binding instead of changing retired history.")
		}
	}
	action := "retrieval_binding.update"
	reply := access.Reply{Status: http.StatusNoContent}
	if retire {
		_, err = tx.Exec(r.Context(), "UPDATE olp.retrieval_bindings SET retired_at=now(),etag=$2 WHERE id=$1", id, access.NewID())
		action = "retrieval_binding.retire"
	} else {
		next := &Binding{Input: in}
		err = Save(r.Context(), tx, p.UserID(), current, next)
		if err == nil {
			id = next.ID
			reply = access.Detail(next, next.ETag)
			if current == nil {
				action = "retrieval_binding.create"
				reply.Status = 201
				reply.Location = "/api/v1/retrieval-bindings/" + id
			}
		}
	}
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return access.Reply{}, access.Fail(409, "retrieval_binding_exists", "Choose an unused binding name in this project.")
		}
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), action, "retrieval_binding", id, "success"); err != nil {
		return access.Reply{}, err
	}
	if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
		return access.Reply{}, err
	}
	if err = s.Access.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}
