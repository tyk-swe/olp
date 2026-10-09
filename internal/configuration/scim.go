package configuration

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/scim"
)

type SCIMGroupMapping struct {
	Name     string               `json:"name"`
	Role     *string              `json:"role"`
	Scope    string               `json:"access_scope"`
	Projects []SCIMProjectMapping `json:"projects"`
}
type SCIMProjectMapping struct {
	Project string `json:"project"`
	Role    string `json:"role"`
}

func (m SCIMGroupMapping) mapping(projects map[string]string) (map[string]any, error) {
	out := map[string]any{"accessScope": m.Scope}
	if m.Role != nil {
		out["role"] = *m.Role
	}
	members := []any{}
	for _, p := range m.Projects {
		id := projects[strings.ToLower(p.Project)]
		if id == "" {
			return nil, access.Invalid("scim_group_mappings", "Every mapped project must exist or be declared.")
		}
		members = append(members, map[string]any{"value": id, "role": p.Role})
	}
	out["projects"] = members
	return out, nil
}

func validateSCIMMappings(d *Document) error {
	if len(d.SCIMGroupMappings) > 1000 {
		return access.Invalid("scim_group_mappings", "At most 1000 group mappings are supported.")
	}
	names := map[string]bool{}
	for i := range d.SCIMGroupMappings {
		m := &d.SCIMGroupMappings[i]

		key := strings.ToLower(m.Name)
		if access.ValidText("name", m.Name, 100) != nil || names[key] {
			return access.Invalid("scim_group_mappings", "Use unique nonempty group names up to 100 characters.")
		}
		names[key] = true
		if m.Scope == "" {
			m.Scope = "assigned"
		}
		if m.Scope != "assigned" && m.Scope != "global" {
			return access.Invalid("scim_group_mappings", "Use assigned or global scope.")
		}
		if m.Role != nil && !slices.Contains([]string{"owner", "operator", "developer", "viewer"}, *m.Role) {
			return access.Invalid("scim_group_mappings", "Unknown installation role.")
		}
		if len(m.Projects) > 100 {
			return access.Invalid("scim_group_mappings", "At most 100 project grants per group.")
		}
		seen := map[string]bool{}
		for _, grant := range m.Projects {
			k := strings.ToLower(grant.Project)
			if access.ValidText("project", grant.Project, 100) != nil || seen[k] || grant.Role != "viewer" && grant.Role != "manager" {
				return access.Invalid("scim_group_mappings", "Use unique project names with viewer or manager roles.")
			}
			seen[k] = true
		}
	}
	return nil
}

func exportSCIMMappings(ctx context.Context, q access.Queryer) ([]SCIMGroupMapping, error) {
	rows, err := q.Query(ctx, `SELECT g.document->>'displayName',g.document->'`+scim.GroupExtension+`'->>'role',COALESCE(g.document->'`+scim.GroupExtension+`'->>'accessScope','assigned'),COALESCE((SELECT jsonb_agg(jsonb_build_object('project',p.name,'role',m.role) ORDER BY p.name) FROM olp.scim_group_projects m JOIN olp.projects p ON p.id=m.project_id WHERE m.group_id=g.id),'[]'::jsonb) FROM olp.scim_groups g ORDER BY lower(g.document->>'displayName')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SCIMGroupMapping
	for rows.Next() {
		var m SCIMGroupMapping
		if err = rows.Scan(&m.Name, &m.Role, &m.Scope, &m.Projects); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func planSCIMMappings(ctx context.Context, q access.Queryer, d *Document, state *stateView, result *planResult) error {
	projects := map[string]string{}
	for k, v := range state.projects {
		projects[k] = v
	}
	for _, p := range d.Projects {
		key := strings.ToLower(p.Name)
		if projects[key] == "" {
			projects[key] = "00000000-0000-0000-0000-000000000001"
		}
	}
	for _, m := range d.SCIMGroupMappings {
		desired, err := m.mapping(projects)
		if err != nil {
			return err
		}
		var current map[string]any
		err = q.QueryRow(ctx, "SELECT document->$2 FROM olp.scim_groups WHERE lower(document->>'displayName')=lower($1)", m.Name, scim.GroupExtension).Scan(&current)
		action := "replace"
		if errors.Is(err, pgx.ErrNoRows) {
			action = "create"
		} else if err != nil {
			return err
		} else if sameSCIMMapping(current, desired) {
			action = "reuse"
		}
		result.item("configuration", "scim_group:"+m.Name, action, "Apply group access mapping; directory identities and memberships remain local.")
	}
	return nil
}

func (s *Server) applySCIMMappings(ctx context.Context, tx pgx.Tx, p access.Principal, d *Document, projects map[string]string) error {
	for _, m := range d.SCIMGroupMappings {
		desired, err := m.mapping(projects)
		if err != nil {
			return err
		}
		var current map[string]any
		err = tx.QueryRow(ctx, "SELECT document->$2 FROM olp.scim_groups WHERE lower(document->>'displayName')=lower($1) FOR UPDATE", m.Name, scim.GroupExtension).Scan(&current)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && sameSCIMMapping(current, desired) {
			continue
		} // comparison uses JSON's common representation
		encoded, _ := json.Marshal(desired)
		if err = json.Unmarshal(encoded, &desired); err != nil {
			return err
		}
		if err = s.Access.StoreSCIMMapping(ctx, tx, p, m.Name, desired); err != nil {
			return err
		}
	}
	return nil
}

func sameSCIMMapping(a, b map[string]any) bool {
	normalize := func(m map[string]any) map[string]any {
		data, _ := json.Marshal(m)
		var out map[string]any
		_ = json.Unmarshal(data, &out)
		if list, ok := out["projects"].([]any); ok {
			slices.SortFunc(list, func(a, b any) int {
				x, _ := a.(map[string]any)
				y, _ := b.(map[string]any)
				return strings.Compare(scim.String(x, "value"), scim.String(y, "value"))
			})
		}
		return out
	}
	return reflect.DeepEqual(normalize(a), normalize(b))
}
