package configuration

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/retrieval"
)

// RetrievalBindingEntry references destination keys by exact project-local
// names. Ambiguous or absent names block promotion; grants are never widened
// to all keys or copied by coincidentally equal UUIDs across installations.
type RetrievalBindingEntry struct {
	Name           string           `json:"name"`
	Project        string           `json:"project"`
	Route          string           `json:"route"`
	TargetProvider string           `json:"target_provider"`
	Backend        string           `json:"backend"`
	Resource       string           `json:"backend_resource"`
	Fields         retrieval.Fields `json:"fields"`
	KeyGrants      []string         `json:"key_grants"`
	Enabled        bool             `json:"enabled"`
}

func retrievalKey(e RetrievalBindingEntry) string {
	return strings.ToLower(e.Project) + "\x00" + strings.ToLower(e.Name)
}
func validateRetrievalBindings(doc *Document) error {
	if len(doc.RetrievalBindings) > 512 {
		return access.Invalid("retrieval_bindings", "Declare at most 512 retrieval bindings.")
	}
	seen := map[string]bool{}
	for i := range doc.RetrievalBindings {
		e := &doc.RetrievalBindings[i]
		e.Name = strings.TrimSpace(e.Name)
		e.Project = strings.TrimSpace(e.Project)
		if !guardrailLabel(e.Project) || !guardrailLabel(e.Name) || !guardrailLabel(e.TargetProvider) || e.Route == "" || len(e.Route) > 128 || seen[retrievalKey(*e)] || !retrieval.ResourceValid(e.Backend, e.Resource) {
			return access.Invalid("retrieval_bindings", "Use unique project/name pairs with an explicit route, provider and native backend resource.")
		}
		seen[retrievalKey(*e)] = true
		if err := retrieval.ValidateFields(e.Backend, e.Fields); err != nil {
			return access.Invalid("retrieval_bindings.fields", err.Error())
		}
		if len(e.KeyGrants) > 2000 {
			return access.Invalid("retrieval_bindings.key_grants", "Use at most 2000 explicit key names.")
		}
		for _, name := range e.KeyGrants {
			if !guardrailLabel(name) {
				return access.Invalid("retrieval_bindings.key_grants", "Use nonempty exact project-local API-key names.")
			}
		}
		slices.Sort(e.KeyGrants)
		e.KeyGrants = slices.Compact(e.KeyGrants)
	}
	return nil
}
func exportRetrievalBindings(ctx context.Context, q access.Queryer) ([]RetrievalBindingEntry, error) {
	bindings, err := retrieval.List(ctx, q)
	if err != nil {
		return nil, err
	}
	out := []RetrievalBindingEntry{}
	for _, b := range bindings {
		e := RetrievalBindingEntry{Name: b.Name, Route: b.Route, Backend: b.Backend, Resource: b.Resource, Fields: b.Fields, Enabled: b.Enabled, KeyGrants: []string{}}
		if err = q.QueryRow(ctx, "SELECT name FROM olp.projects WHERE id=$1", b.ProjectID).Scan(&e.Project); err != nil {
			return nil, err
		}
		err = q.QueryRow(ctx, "SELECT name FROM olp.providers WHERE id=$1", b.ProviderID).Scan(&e.TargetProvider)
		if err != nil {
			return nil, err
		}
		for _, id := range b.KeyGrants {
			var name string
			err = q.QueryRow(ctx, "SELECT name FROM olp.api_keys WHERE id=$1 AND project_id=$2", id, b.ProjectID).Scan(&name)
			if err != nil {
				return nil, err
			}
			e.KeyGrants = append(e.KeyGrants, name)
		}
		slices.Sort(e.KeyGrants)
		out = append(out, e)
	}
	return out, nil
}
func resolveRetrievalEntry(ctx context.Context, q access.Queryer, e RetrievalBindingEntry) (retrieval.Input, error) {
	in := retrieval.Input{Name: e.Name, Route: e.Route, Backend: e.Backend, Resource: e.Resource, Fields: e.Fields, Enabled: e.Enabled, KeyGrants: []string{}}
	if err := q.QueryRow(ctx, "SELECT id::text FROM olp.projects WHERE lower(name)=lower($1)", e.Project).Scan(&in.ProjectID); err != nil {
		return in, err
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT t->>'id' FROM (
 SELECT d.targets,0 AS priority FROM olp.route_drafts d WHERE d.slug=$1 AND d.project_id=$2 AND d.state='draft'
 UNION ALL SELECT v.targets,1 AS priority FROM olp.routes r JOIN olp.route_revisions v ON v.id=r.latest_revision_id WHERE r.slug=$1 AND r.project_id=$2 AND r.state='active'
 ) r CROSS JOIN LATERAL jsonb_array_elements(r.targets) t JOIN olp.providers p ON p.id=(t->>'provider_id')::uuid
 WHERE lower(p.name)=lower($3) AND t->>'provider_model'=$4 AND r.priority=(SELECT min(priority) FROM (
 SELECT 0 AS priority FROM olp.route_drafts d WHERE d.slug=$1 AND d.project_id=$2 AND d.state='draft'
 UNION ALL SELECT 1 FROM olp.routes r WHERE r.slug=$1 AND r.project_id=$2 AND r.state='active') priorities)`, in.Route, in.ProjectID, e.TargetProvider, e.Resource)
	if err != nil {
		return in, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return in, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return in, err
	}
	if len(ids) != 1 {
		return in, access.Invalid("retrieval_bindings.target_provider", "The destination route must contain exactly one matching provider/resource target.")
	}
	in.TargetID = ids[0]
	for _, name := range e.KeyGrants {
		var grants []string
		rows, err := q.Query(ctx, "SELECT id::text FROM olp.api_keys WHERE project_id=$1 AND name=$2 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now())", in.ProjectID, name)
		if err != nil {
			return in, err
		}
		for rows.Next() {
			var key string
			if err = rows.Scan(&key); err != nil {
				rows.Close()
				return in, err
			}
			grants = append(grants, key)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return in, err
		}
		if len(grants) != 1 {
			return in, access.Invalid("retrieval_bindings.key_grants", "Each destination grant must resolve to exactly one active API key with the exact project-local name.")
		}
		in.KeyGrants = append(in.KeyGrants, grants[0])
	}
	slices.Sort(in.KeyGrants)
	return in, nil
}
func planRetrievalBindings(ctx context.Context, q access.Queryer, doc *Document, result *planResult) error {
entryLoop:
	for _, e := range doc.RetrievalBindings {
		// Applying a document stages routes before storing bindings. Match
		// their portable provider/resource identity here; Save validates the
		// concrete destination target after those drafts have been written.
		for _, route := range doc.Routes {
			if route.Slug != e.Route {
				continue
			}
			matches := 0
			for _, target := range route.Targets {
				if strings.EqualFold(target.Provider, e.TargetProvider) && target.ProviderModel == e.Resource && target.Shadow == nil {
					matches++
				}
			}
			if route.Retired || route.Project == nil || !strings.EqualFold(*route.Project, e.Project) || !slices.Contains(route.Operations, "retrieval") || matches != 1 {
				result.blocker("retrieval_binding", e.Name, "The document must stage exactly one matching retrieval provider/resource target in the owning project.")
				continue entryLoop
			}
			for _, provider := range doc.Providers {
				if strings.EqualFold(provider.Name, e.TargetProvider) && provider.Configuration.Kind != retrieval.ProviderKind(e.Backend) {
					result.blocker("retrieval_binding", e.Name, "The target provider kind does not match the retrieval backend.")
					continue entryLoop
				}
			}
			var project string
			err := q.QueryRow(ctx, "SELECT id::text FROM olp.projects WHERE lower(name)=lower($1)", e.Project).Scan(&project)
			if errors.Is(err, pgx.ErrNoRows) {
				declared := false
				for _, p := range doc.Projects {
					declared = declared || strings.EqualFold(p.Name, e.Project)
				}
				if !declared || len(e.KeyGrants) > 0 {
					result.blocker("retrieval_binding", e.Name, "Create the destination project and named API-key grants before promoting access.")
					continue entryLoop
				}
			} else if err != nil {
				return err
			}
			for _, name := range e.KeyGrants {
				var count int
				if err = q.QueryRow(ctx, "SELECT count(*) FROM olp.api_keys WHERE project_id=$1 AND name=$2 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now())", project, name).Scan(&count); err != nil {
					return err
				}
				if count != 1 {
					result.blocker("retrieval_binding", e.Name, "Each grant must resolve to exactly one active destination API key by its project-local name.")
					continue entryLoop
				}
			}
			result.item("retrieval_binding", e.Name, "stage", "Bind the staged destination route target and explicit project-local key grants; queries require a published certified target.")
			continue entryLoop
		}
		in, err := resolveRetrievalEntry(ctx, q, e)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				result.blocker("retrieval_binding", e.Name, "Create the destination project and named API-key grants before applying this binding.")
				continue
			}
			var invalid *access.Problem
			if errors.As(err, &invalid) {
				result.blocker("retrieval_binding", e.Name, invalid.Detail)
				continue
			}
			return err
		}
		current, err := retrieval.Find(ctx, q, in.ProjectID, in.Name)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		action := "create"
		if current != nil {
			action = "replace"
			if reflect.DeepEqual(current.Input, in) {
				action = "reuse"
			}
		}
		result.item("retrieval_binding", e.Name, action, "Retain immutable revision history, explicit key grants and the configured backend resource; serving still requires a published certified target.")
	}
	return nil
}
func (s *Server) applyRetrievalBindings(ctx context.Context, tx pgx.Tx, p access.Principal, doc *Document) error {
	for _, e := range doc.RetrievalBindings {
		in, err := resolveRetrievalEntry(ctx, tx, e)
		if err != nil {
			return err
		}
		if err = p.Authorize(access.Configure); err != nil {
			return err
		}
		if err = p.Project(&in.ProjectID, access.Change); err != nil {
			return err
		}
		current, err := retrieval.Find(ctx, tx, in.ProjectID, in.Name)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if current != nil {
			current, err = retrieval.Load(ctx, tx, current.ID, true)
			if err != nil {
				return err
			}
			if reflect.DeepEqual(current.Input, in) {
				continue
			}
		}
		next := &retrieval.Binding{Input: in}
		if err = retrieval.Save(ctx, tx, p.UserID(), current, next); err != nil {
			return err
		}
	}
	if len(doc.RetrievalBindings) > 0 {
		_, err := access.AdvanceAuthority(ctx, tx)
		return err
	}
	return nil
}
