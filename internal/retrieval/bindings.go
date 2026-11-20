package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/retrieval/identity"
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

func normalizeInput(in Input) (Input, error) {
	name := strings.TrimSpace(in.Name)
	route := strings.TrimSpace(in.Route)
	if err := httpkit.ValidText("name", name, 100); err != nil {
		return Input{}, err
	}
	if _, err := uuid.Parse(in.ProjectID); err != nil {
		return Input{}, httpkit.Invalid("project_id", "Choose an owning project.")
	}
	if _, err := uuid.Parse(in.TargetID); err != nil {
		return Input{}, httpkit.Invalid("target_id", "Choose one route target.")
	}
	if route == "" || len(route) > 128 || !identity.ValidResource(in.Backend, in.Resource) {
		return Input{}, httpkit.Invalid("backend_resource", "Choose a supported backend, route and native resource.")
	}
	if err := ValidateFields(in.Backend, in.Fields); err != nil {
		return Input{}, httpkit.Invalid("fields", err.Error())
	}
	if len(in.KeyGrants) > 2000 {
		return Input{}, httpkit.Invalid("key_grants", "Use at most 2000 explicit API-key grants.")
	}
	for _, key := range in.KeyGrants {
		if parsed, err := uuid.Parse(key); err != nil || parsed.String() != key {
			return Input{}, httpkit.Invalid("key_grants", "Use canonical API-key UUIDs.")
		}
	}
	return Input{
		ProjectID: in.ProjectID,
		Name:      name,
		Route:     route,
		TargetID:  in.TargetID,
		Backend:   in.Backend,
		Resource:  in.Resource,
		Fields: Fields{
			Text:     in.Fields.Text,
			ID:       in.Fields.ID,
			Title:    in.Fields.Title,
			URL:      in.Fields.URL,
			Search:   slices.Clone(in.Fields.Search),
			Metadata: maps.Clone(in.Fields.Metadata),
		},
		KeyGrants: append([]string{}, slices.Compact(slices.Sorted(slices.Values(in.KeyGrants)))...),
		Enabled:   in.Enabled,
	}, nil
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
func Load(ctx context.Context, q database.Queryer, id string, lock bool) (*Binding, error) {
	query := "SELECT " + bindingColumns + bindingJoin + " WHERE b.id=$1"
	if lock {
		query += " FOR UPDATE OF b"
	}
	return scanBinding(q.QueryRow(ctx, query, id))
}
func Find(ctx context.Context, q database.Queryer, project, name string) (*Binding, error) {
	return scanBinding(q.QueryRow(ctx, "SELECT "+bindingColumns+bindingJoin+" WHERE b.project_id=$1 AND lower(b.name)=lower($2) AND b.retired_at IS NULL", project, name))
}
func List(ctx context.Context, q database.Queryer) ([]Binding, error) {
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
func ValidateAuthority(ctx context.Context, q database.Queryer, in Input) (string, error) {
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
		return "", httpkit.Invalid("target_id", "Choose a retrieval target in this project's route or draft with the matching backend kind and resource.")
	}
	var count int
	err = q.QueryRow(ctx, "SELECT count(*) FROM olp.api_keys WHERE id=ANY($1::uuid[]) AND project_id=$2 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now())", in.KeyGrants, in.ProjectID).Scan(&count)
	if err != nil {
		return "", err
	}
	if count != len(in.KeyGrants) {
		return "", httpkit.Invalid("key_grants", "Every grant must name an active API key in this project.")
	}
	return providers[0], nil
}
func Save(ctx context.Context, tx pgx.Tx, actor string, current *Binding, in Input) (Binding, error) {
	normalized, err := normalizeInput(in)
	if err != nil {
		return Binding{}, err
	}
	provider, err := ValidateAuthority(ctx, tx, normalized)
	if err != nil {
		return Binding{}, err
	}
	if current != nil && (current.RetiredAt != nil || current.ProjectID != normalized.ProjectID) {
		return Binding{}, httpkit.Invalid("project_id", "The binding project is immutable and retired bindings cannot change.")
	}
	id, revision := nextBindingIdentity(current)
	next := Binding{
		Input:      normalized,
		ProviderID: provider,
		ID:         id,
		RevisionID: httpkit.NewID(),
		Revision:   revision,
		ETag:       httpkit.NewID(),
	}
	raw, err := json.Marshal(struct {
		Input
		ProviderID string `json:"provider_id"`
	}{next.Input, next.ProviderID})
	if err != nil {
		return Binding{}, err
	}
	if current == nil {
		if _, err := tx.Exec(ctx, "INSERT INTO olp.retrieval_bindings(id,project_id,name,latest_revision_id,etag,created_by) VALUES($1,$2,$3,$4,$5,$6)", next.ID, next.ProjectID, next.Name, next.RevisionID, next.ETag, actor); err != nil {
			return Binding{}, err
		}
	} else {
		if _, err := tx.Exec(ctx, "UPDATE olp.retrieval_bindings SET name=$2,latest_revision_id=$3,etag=$4 WHERE id=$1", next.ID, next.Name, next.RevisionID, next.ETag); err != nil {
			return Binding{}, err
		}
	}
	if _, err := tx.Exec(ctx, "INSERT INTO olp.retrieval_binding_revisions(id,binding_id,revision,definition,created_by) VALUES($1,$2,$3,$4,$5)", next.RevisionID, next.ID, next.Revision, raw, actor); err != nil {
		return Binding{}, err
	}
	return next, nil
}

func nextBindingIdentity(current *Binding) (string, int64) {
	if current != nil {
		return current.ID, current.Revision + 1
	}
	return httpkit.NewID(), 1
}
func (s *Server) Register(mux *http.ServeMux) {
	s.Access.Route(mux, "GET /api/v1/retrieval-bindings", s.list)
	s.Access.Route(mux, "POST /api/v1/retrieval-bindings", s.write)
	s.Access.Route(mux, "GET /api/v1/retrieval-bindings/{binding_id}", s.get)
	s.Access.Route(mux, "PUT /api/v1/retrieval-bindings/{binding_id}", s.write)
	s.Access.Route(mux, "DELETE /api/v1/retrieval-bindings/{binding_id}", s.write)
	s.Access.Route(mux, "GET /api/v1/retrieval-bindings/{binding_id}/revisions/{revision_id}", s.revision)
}
func (s *Server) list(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+bindingColumns+bindingJoin+" WHERE b.retired_at IS NULL AND b.id<$1 AND ($2 OR b.project_id=ANY($3::uuid[])) ORDER BY b.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		d, err := scanBinding(rows)
		if err != nil {
			return httpkit.Reply{}, err
		}
		raw, _ := json.Marshal(d)
		var item map[string]any
		_ = json.Unmarshal(raw, &item)
		items = append(items, item)
	}
	return httpkit.ListReply(items, page), rows.Err()
}
func (s *Server) get(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "binding_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	d, err := Load(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Project(&d.ProjectID, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(d, d.ETag), nil
}
func (s *Server) revision(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "binding_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	version, err := httpkit.IDParam(r, "revision_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	d, err := Load(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Project(&d.ProjectID, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	var raw json.RawMessage
	var n int64
	err = s.Access.Pool.QueryRow(r.Context(), "SELECT definition,revision FROM olp.retrieval_binding_revisions WHERE id=$1 AND binding_id=$2", version, id).Scan(&raw, &n)
	if err != nil {
		return httpkit.Reply{}, err
	}
	var definition Input
	if err = json.Unmarshal(raw, &definition); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(map[string]any{"id": version, "binding_id": id, "revision": n, "definition": definition}, version), nil
}
func readBindingInput(r *http.Request) (Input, error) {
	if r.Method == http.MethodDelete {
		return Input{}, nil
	}
	var in Input
	if err := httpkit.DecodeUnique(r, &in, 64<<10); err != nil {
		return Input{}, err
	}
	return normalizeInput(in)
}

func (s *Server) write(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	in, err := readBindingInput(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(s.Access, r, in, s.target(r, in), func(tx pgx.Tx, p authz.Principal, current *Binding) (httpkit.Reply, access.Event, error) {
		if current != nil {
			if err := httpkit.Match(r, current.ETag); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			if current.RetiredAt != nil {
				return httpkit.Reply{}, access.Event{}, httpkit.Fail(409, "retrieval_binding_retired", "Create a new binding instead of changing retired history.")
			}
		}
		reply, event, err := change(r, tx, p, current, in)
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(409, "retrieval_binding_exists", "Choose an unused binding name in this project.")
		}
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		_, err = access.AdvanceAuthority(r.Context(), tx)
		return reply, event, err
	})
}

func (s *Server) target(r *http.Request, in Input) func(pgx.Tx, authz.Principal) (*Binding, error) {
	return func(tx pgx.Tx, p authz.Principal) (*Binding, error) {
		if err := p.Authorize(authz.Configure); err != nil {
			return nil, err
		}
		if r.PathValue("binding_id") == "" {
			return nil, s.Access.RequireProject(r.Context(), tx, p, &in.ProjectID)
		}
		id, err := httpkit.IDParam(r, "binding_id")
		if err != nil {
			return nil, err
		}
		current, err := Load(r.Context(), tx, id, true)
		if err != nil {
			return nil, err
		}
		return current, p.Project(&current.ProjectID, authz.Change)
	}
}

func change(r *http.Request, tx pgx.Tx, p authz.Principal, current *Binding, in Input) (httpkit.Reply, access.Event, error) {
	if r.Method == http.MethodDelete {
		_, err := tx.Exec(r.Context(), "UPDATE olp.retrieval_bindings SET retired_at=now(),etag=$2 WHERE id=$1", current.ID, httpkit.NewID())
		return httpkit.Reply{Status: http.StatusNoContent}, bindingEvent("retire", current.ID), err
	}
	next, err := Save(r.Context(), tx, p.UserID(), current, in)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	reply := httpkit.Detail(next, next.ETag)
	if current != nil {
		return reply, bindingEvent("update", next.ID), nil
	}
	reply.Status, reply.Location = http.StatusCreated, "/api/v1/retrieval-bindings/"+next.ID
	return reply, bindingEvent("create", next.ID), nil
}

func bindingEvent(action, id string) access.Event {
	return access.Event{Action: "retrieval_binding." + action, Resource: "retrieval_binding", ID: id}
}
