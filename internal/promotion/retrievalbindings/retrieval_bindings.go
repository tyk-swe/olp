package retrievalbindings

import (
	"context"
	"errors"
	"github.com/tyk-swe/olp/internal/promotion"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/retrieval"
)

func ValidateAndNormalizeRetrievalBindings(doc *promotion.Document) error {
	if len(doc.RetrievalBindings) > 512 {
		return access.Invalid("retrieval_bindings", "Declare at most 512 retrieval bindings.")
	}
	seen := map[string]bool{}
	for i := range doc.RetrievalBindings {
		e := &doc.RetrievalBindings[i]
		e.Name = strings.TrimSpace(e.Name)
		e.Project = strings.TrimSpace(e.Project)
		if !promotion.GuardrailLabel(e.Project) || !promotion.GuardrailLabel(e.Name) || !promotion.GuardrailLabel(e.TargetProvider) || e.Route == "" || len(e.Route) > 128 || seen[promotion.RetrievalKey(*e)] || !retrieval.ResourceValid(e.Backend, e.Resource) {
			return access.Invalid("retrieval_bindings", "Use unique project/name pairs with an explicit route, provider and native backend resource.")
		}
		seen[promotion.RetrievalKey(*e)] = true
		if err := retrieval.ValidateFields(e.Backend, e.Fields); err != nil {
			return access.Invalid("retrieval_bindings.fields", err.Error())
		}
		if len(e.KeyGrants) > 2000 {
			return access.Invalid("retrieval_bindings.key_grants", "Use at most 2000 explicit key names.")
		}
		for _, name := range e.KeyGrants {
			if !promotion.GuardrailLabel(name) {
				return access.Invalid("retrieval_bindings.key_grants", "Use nonempty exact project-local API-key names.")
			}
		}
		if len(e.KeyGrants) > 0 {
			e.KeyGrants = slices.Compact(slices.Sorted(slices.Values(e.KeyGrants)))
		}
	}
	return nil
}
func ExportRetrievalBindings(ctx context.Context, q access.Queryer) ([]promotion.RetrievalBindingEntry, error) {
	bindings, err := retrieval.List(ctx, q)
	if err != nil {
		return nil, err
	}
	out := []promotion.RetrievalBindingEntry{}
	for _, b := range bindings {
		e := promotion.RetrievalBindingEntry{Name: b.Name, Route: b.Route, Backend: b.Backend, Resource: b.Resource, Fields: b.Fields, Enabled: b.Enabled, KeyGrants: []string{}}
		if err = q.QueryRow(ctx, "SELECT name FROM olp.projects WHERE id=$1", b.ProjectID).Scan(&e.Project); err != nil {
			return nil, err
		}
		err = q.QueryRow(ctx, "SELECT name FROM olp.providers WHERE id=$1", b.ProviderID).Scan(&e.TargetProvider)
		if err != nil {
			return nil, err
		}
		for _, id := range b.KeyGrants {
			var name string
			err = q.QueryRow(ctx, "SELECT name FROM olp.api_keys WHERE id=$1 AND project_id=$2 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now())", id, b.ProjectID).Scan(&name)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, access.Invalid("retrieval_bindings.key_grants", "Remove inactive API-key grants before exporting retrieval bindings.")
			}
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
func resolveRetrievalEntry(ctx context.Context, q access.Queryer, e promotion.RetrievalBindingEntry) (retrieval.Input, error) {
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
func PlanRetrievalBindings(ctx context.Context, q access.Queryer, doc *promotion.Document, result *promotion.PlanResult) error {
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
				result.Blocker("retrieval_binding", e.Name, "The document must stage exactly one matching retrieval provider/resource target in the owning project.")
				continue entryLoop
			}
			for _, provider := range doc.Providers {
				if strings.EqualFold(provider.Name, e.TargetProvider) && provider.Configuration.Kind != retrieval.ProviderKind(e.Backend) {
					result.Blocker("retrieval_binding", e.Name, "The target provider kind does not match the retrieval backend.")
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
					result.Blocker("retrieval_binding", e.Name, "Create the destination project and named API-key grants before promoting access.")
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
					result.Blocker("retrieval_binding", e.Name, "Each grant must resolve to exactly one active destination API key by its project-local name.")
					continue entryLoop
				}
			}
			result.Add("retrieval_binding", e.Name, "stage", "Bind the staged destination route target and explicit project-local key grants; queries require a published certified target.")
			continue entryLoop
		}
		in, err := resolveRetrievalEntry(ctx, q, e)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				result.Blocker("retrieval_binding", e.Name, "Create the destination project and named API-key grants before applying this binding.")
				continue
			}
			var invalid *access.Problem
			if errors.As(err, &invalid) {
				result.Blocker("retrieval_binding", e.Name, invalid.Detail)
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
		result.Add("retrieval_binding", e.Name, action, "Retain immutable revision history, explicit key grants and the configured backend resource; serving still requires a published certified target.")
	}
	return nil
}
func ApplyRetrievalBindings(s *promotion.Server, ctx context.Context, tx pgx.Tx, p access.Principal, doc *promotion.Document) error {
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
		if _, err := retrieval.Save(ctx, tx, p.UserID(), current, in); err != nil {
			return err
		}
	}
	if len(doc.RetrievalBindings) > 0 {
		_, err := access.AdvanceAuthority(ctx, tx)
		return err
	}
	return nil
}
