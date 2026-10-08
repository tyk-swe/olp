package configuration

import (
	"context"
	"errors"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/workload"
)

// WorkloadIssuerEntry replaces installation-local project IDs with project names.
// Principals, digests, signing keys and creator identities are never promoted.
type WorkloadIssuerEntry struct {
	workload.Config
	Mappings []WorkloadMappingEntry `json:"mappings"`
}
type WorkloadMappingEntry struct {
	Name               string            `json:"name"`
	Match              map[string]string `json:"match"`
	Project            string            `json:"project"`
	LimitTemplate      string            `json:"limit_template"`
	RouteGroups        []string          `json:"route_groups"`
	Scopes             []string          `json:"scopes"`
	EndUserClaim       *string           `json:"end_user_claim"`
	AllowProviderState bool              `json:"allow_provider_state"`
}

func (e WorkloadIssuerEntry) config(projects map[string]string) (workload.Config, error) {
	c := e.Config
	c.Mappings = make([]workload.Mapping, 0, len(e.Mappings))
	for _, m := range e.Mappings {
		id, ok := projects[strings.ToLower(m.Project)]
		if !ok {
			return c, access.Invalid("workload_issuers", "Every mapping must reference a declared or existing project.")
		}
		c.Mappings = append(c.Mappings, workload.Mapping{Name: m.Name, Match: m.Match, ProjectID: id, LimitTemplate: m.LimitTemplate, RouteGroups: m.RouteGroups, Scopes: m.Scopes, EndUserClaim: m.EndUserClaim, AllowProviderState: m.AllowProviderState})
	}
	return c, access.ValidateWorkloadConfig(c)
}
func exportWorkloadIssuers(ctx context.Context, q access.Queryer) ([]WorkloadIssuerEntry, error) {
	rows, err := q.Query(ctx, "SELECT document FROM olp.workload_issuers ORDER BY issuer")
	if err != nil {
		return nil, err
	}
	var configs []workload.Config
	for rows.Next() {
		var c workload.Config
		if err = rows.Scan(&c); err != nil {
			rows.Close()
			return nil, err
		}
		configs = append(configs, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(configs) == 0 {
		return nil, nil
	}
	ids := map[string]bool{}
	for _, c := range configs {
		for _, m := range c.Mappings {
			ids[m.ProjectID] = true
		}
	}
	wanted := make([]string, 0, len(ids))
	for id := range ids {
		wanted = append(wanted, id)
	}
	projectRows, err := q.Query(ctx, "SELECT id::text,name FROM olp.projects WHERE id=ANY($1::uuid[])", wanted)
	if err != nil {
		return nil, err
	}
	projects := map[string]string{}
	for projectRows.Next() {
		var id, name string
		if err = projectRows.Scan(&id, &name); err != nil {
			projectRows.Close()
			return nil, err
		}
		projects[id] = name
	}
	projectRows.Close()
	if err = projectRows.Err(); err != nil {
		return nil, err
	}
	var result []WorkloadIssuerEntry
	for _, c := range configs {
		e := WorkloadIssuerEntry{Config: c, Mappings: make([]WorkloadMappingEntry, 0, len(c.Mappings))}
		e.Config.Mappings = nil
		for _, m := range c.Mappings {
			project, ok := projects[m.ProjectID]
			if !ok {
				return nil, access.Invalid("workload_issuers", "A workload mapping project is no longer available.")
			}
			e.Mappings = append(e.Mappings, WorkloadMappingEntry{Name: m.Name, Match: m.Match, Project: project, LimitTemplate: m.LimitTemplate, RouteGroups: m.RouteGroups, Scopes: m.Scopes, EndUserClaim: m.EndUserClaim, AllowProviderState: m.AllowProviderState})
		}
		result = append(result, e)
	}
	return result, nil
}
func validateWorkloadEntries(doc *Document) error {
	if len(doc.WorkloadIssuers) > 64 {
		return access.Invalid("workload_issuers", "Use at most 64 issuers.")
	}
	seen := map[string]bool{}
	for i := range doc.WorkloadIssuers {
		e := &doc.WorkloadIssuers[i]
		if seen[e.Issuer] {
			return access.Invalid("workload_issuers", "Issuer URLs must be unique.")
		}
		seen[e.Issuer] = true
		projects := map[string]string{}
		for j := range e.Mappings {
			m := &e.Mappings[j]
			m.Project = strings.TrimSpace(m.Project)
			if err := access.ValidText("workload_issuers", m.Project, 100); err != nil {
				return err
			}
			projects[strings.ToLower(m.Project)] = "00000000-0000-0000-0000-000000000001"
		}
		if _, err := e.config(projects); err != nil {
			return err
		}
	}
	return nil
}
func planWorkloadIssuers(ctx context.Context, q access.Queryer, doc *Document, state *stateView, result *planResult) error {
	projects := make(map[string]string, len(state.projects)+len(doc.Projects))
	for name, id := range state.projects {
		projects[name] = id
	}
	for _, p := range doc.Projects {
		if projects[strings.ToLower(p.Name)] == "" {
			projects[strings.ToLower(p.Name)] = "00000000-0000-0000-0000-000000000001"
		}
	}
	for _, e := range doc.WorkloadIssuers {
		c, err := e.config(projects)
		if err != nil {
			return err
		}
		// Declared project policies take precedence; undeclared projects retain theirs.
		if e.Enabled {
			for _, m := range e.Mappings {
				templates := state.projectTemplates[projects[strings.ToLower(m.Project)]]
				groups := state.projectRouteGroups[projects[strings.ToLower(m.Project)]]
				for _, p := range doc.Projects {
					if strings.EqualFold(p.Name, m.Project) {
						if p.LimitTemplates != nil {
							templates = *p.LimitTemplates
						}
						groups = p.RouteGroups
						break
					}
				}
				if _, ok := templates[m.LimitTemplate]; !ok {
					return access.Invalid("workload_issuers", "A mapping template is absent from its project.")
				}
				for _, group := range m.RouteGroups {
					if _, ok := groups[group]; !ok {
						return access.Invalid("workload_issuers", "A mapping route group is absent from its project.")
					}
				}
			}
		}
		var current workload.Config
		var id string
		err = q.QueryRow(ctx, "SELECT id::text,document FROM olp.workload_issuers WHERE issuer=$1", c.Issuer).Scan(&id, &current)
		action := "reuse"
		if errors.Is(err, pgx.ErrNoRows) {
			action = "create"
		} else if err != nil {
			return err
		} else if !reflect.DeepEqual(current, c) {
			action = "replace"
		}
		if id != "" {
			if err := access.ValidateWorkloadBindings(ctx, q, id, c); err != nil {
				return err
			}
		}
		result.item("configuration", "workload_issuer:"+c.Issuer, action, "Issuer trust and claim mappings; destination workload principals are preserved.")
	}
	return nil
}

// prepareWorkloadIssuers runs before the installation mutation lock. Revocation
// and unchanged endpoint edits must remain available during a JWKS outage.
func (s *Server) prepareWorkloadIssuers(ctx context.Context, p access.Principal, doc *Document) error {
	if len(doc.WorkloadIssuers) == 0 {
		return nil
	}
	if err := p.Authorize(access.Access); err != nil {
		return err
	}
	if err := validateWorkloadEntries(doc); err != nil {
		return err
	}
	doc.preparedWorkloads = map[string]string{}
	for _, e := range doc.WorkloadIssuers {
		var old workload.Config
		var etag string
		err := s.Access.Pool.QueryRow(ctx, "SELECT document,etag::text FROM olp.workload_issuers WHERE issuer=$1", e.Issuer).Scan(&old, &etag)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		doc.preparedWorkloads[e.Issuer] = etag
		if !e.Enabled {
			continue
		}
		if err == nil && old.Enabled && old.JWKSURL == e.JWKSURL {
			continue
		}
		if _, err = workload.Fetch(ctx, s.Access.OIDCClient, e.JWKSURL); err != nil {
			return access.Fail(422, "workload_keys_unavailable", "The declared public signing keys could not be verified through identity egress.")
		}
	}
	return nil
}
func (s *Server) applyWorkloadIssuers(ctx context.Context, tx pgx.Tx, p access.Principal, doc *Document, projects map[string]string) error {
	for _, e := range doc.WorkloadIssuers {
		c, err := e.config(projects)
		if err != nil {
			return err
		}
		var current workload.Config
		var id, etag string
		err = tx.QueryRow(ctx, "SELECT id::text,document,etag::text FROM olp.workload_issuers WHERE issuer=$1 FOR UPDATE", c.Issuer).Scan(&id, &current, &etag)
		create := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !create {
			return err
		}
		if version, prepared := doc.preparedWorkloads[c.Issuer]; !prepared || version != etag {
			return access.Fail(409, "configuration_changed", "Workload trust changed during endpoint validation; plan and retry.")
		}
		if !create && reflect.DeepEqual(c, current) {
			continue
		}
		if create {
			id = access.NewID()
		}
		if _, err = s.Access.StoreWorkloadIssuer(ctx, tx, p, id, c, create); err != nil {
			return err
		}
	}
	return nil
}
