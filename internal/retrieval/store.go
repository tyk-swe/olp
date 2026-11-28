package retrieval

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/textfield"
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

type revisionDetail struct {
	BindingID  string `json:"binding_id"`
	Definition Input  `json:"definition"`
	ID         string `json:"id"`
	Revision   int64  `json:"revision"`
}

func normalizeInput(in Input) (Input, error) {
	name := strings.TrimSpace(in.Name)
	route := strings.TrimSpace(in.Route)
	if err := textfield.Validate("name", name, 100); err != nil {
		return Input{}, err
	}
	if _, err := uuid.Parse(in.ProjectID); err != nil {
		return Input{}, problem.Invalid("project_id", "Choose an owning project.")
	}
	if _, err := uuid.Parse(in.TargetID); err != nil {
		return Input{}, problem.Invalid("target_id", "Choose one route target.")
	}
	if route == "" || len(route) > 128 || !identity.ValidResource(in.Backend, in.Resource) {
		return Input{}, problem.Invalid("backend_resource", "Choose a supported backend, route and native resource.")
	}
	if err := ValidateFields(in.Backend, in.Fields); err != nil {
		return Input{}, problem.Invalid("fields", err.Error())
	}
	if len(in.KeyGrants) > 2000 {
		return Input{}, problem.Invalid("key_grants", "Use at most 2000 explicit API-key grants.")
	}
	for _, key := range in.KeyGrants {
		if !ids.CanonicalOrNil(key) {
			return Input{}, problem.Invalid("key_grants", "Use canonical API-key UUIDs.")
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

func listPage(ctx context.Context, q database.Queryer, page database.Pagination, allProjects bool, projects []string) ([]*Binding, error) {
	rows, err := q.Query(ctx, "SELECT "+bindingColumns+bindingJoin+" WHERE b.retired_at IS NULL AND b.id<$1 AND ($2 OR b.project_id=ANY($3::uuid[])) ORDER BY b.id DESC LIMIT $4", page.Before, allProjects, projects, page.Limit+1)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Binding, error) { return scanBinding(row) })
}

func loadRevision(ctx context.Context, q database.Queryer, bindingID, revisionID string) (revisionDetail, error) {
	var raw []byte
	var number int64
	if err := q.QueryRow(ctx, "SELECT definition,revision FROM olp.retrieval_binding_revisions WHERE id=$1 AND binding_id=$2", revisionID, bindingID).Scan(&raw, &number); err != nil {
		return revisionDetail{}, err
	}
	var definition Input
	if err := json.Unmarshal(raw, &definition); err != nil {
		return revisionDetail{}, err
	}
	return revisionDetail{BindingID: bindingID, Definition: definition, ID: revisionID, Revision: number}, nil
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
		return "", problem.Invalid("target_id", "Choose a retrieval target in this project's route or draft with the matching backend kind and resource.")
	}
	var count int
	err = q.QueryRow(ctx, "SELECT count(*) FROM olp.api_keys WHERE id=ANY($1::uuid[]) AND project_id=$2 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now())", in.KeyGrants, in.ProjectID).Scan(&count)
	if err != nil {
		return "", err
	}
	if count != len(in.KeyGrants) {
		return "", problem.Invalid("key_grants", "Every grant must name an active API key in this project.")
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
		return Binding{}, problem.Invalid("project_id", "The binding project is immutable and retired bindings cannot change.")
	}
	id, revision := nextBindingIdentity(current)
	next := Binding{
		Input:      normalized,
		ProviderID: provider,
		ID:         id,
		RevisionID: ids.New(),
		Revision:   revision,
		ETag:       ids.New(),
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
	return ids.New(), 1
}

func markRetired(ctx context.Context, tx pgx.Tx, id, etag string) error {
	_, err := tx.Exec(ctx, "UPDATE olp.retrieval_bindings SET retired_at=now(),etag=$2 WHERE id=$1", id, etag)
	return err
}
