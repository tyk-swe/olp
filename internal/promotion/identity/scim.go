package identity

import (
	"cmp"
	"context"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/scim"
)

type grant = struct {
	Project string                                             `json:"project"`
	Role    contract.ConfigurationSCIMGroupMappingProjectsRole `json:"role"`
}

type group = contract.ConfigurationSCIMGroupMapping

const (
	readGroups     = `SELECT g.document->>'displayName',g.document->'` + scim.GroupExtension + `'->>'role',COALESCE(g.document->'` + scim.GroupExtension + `'->>'accessScope','assigned'),COALESCE((SELECT jsonb_agg(jsonb_build_object('project',p.name,'role',m.role) ORDER BY p.name) FROM olp.scim_group_projects m JOIN olp.projects p ON p.id=m.project_id WHERE m.group_id=g.id),'[]'::jsonb) FROM olp.scim_groups g ORDER BY lower(g.document->>'displayName')`
	missingProject = "Every mapped project must exist or be declared."
)

func parseSCIM(fragment nullable.Nullable[[]group]) ([]group, error) {
	entries, _ := promotion.Present(fragment)
	if len(entries) > 1000 {
		return nil, access.Invalid("scim_group_mappings", "At most 1000 group mappings are supported.")
	}
	if !unique(entries, func(g group) string { return folded(g.Name) }) {
		return nil, access.Invalid("scim_group_mappings", "Use unique nonempty group names up to 100 characters.")
	}
	if err := firstError(entries, validGroup); err != nil {
		return nil, err
	}
	return mapped(entries, func(g group) group {
		return group{Name: g.Name, Role: g.Role, Projects: g.Projects, AccessScope: cmp.Or(g.AccessScope, contract.ConfigurationSCIMGroupMappingAccessScopeAssigned)}
	}), nil
}

func validGroup(g group) error {
	role, hasRole := promotion.Present(g.Role)
	switch {
	case access.ValidText("name", g.Name, 100) != nil:
		return access.Invalid("scim_group_mappings", "Use unique nonempty group names up to 100 characters.")
	case g.AccessScope != "" && !g.AccessScope.Valid():
		return access.Invalid("scim_group_mappings", "Use assigned or global scope.")
	case hasRole && !installationRole(role):
		return access.Invalid("scim_group_mappings", "Unknown installation role.")
	case len(g.Projects) > 100:
		return access.Invalid("scim_group_mappings", "At most 100 project grants per group.")
	case !unique(g.Projects, func(p grant) string { return folded(p.Project) }) || slices.ContainsFunc(g.Projects, invalidGrant):
		return access.Invalid("scim_group_mappings", "Use unique project names with viewer or manager roles.")
	}
	return nil
}

func installationRole(role contract.ConfigurationSCIMGroupMappingRole) bool {
	switch role {
	case contract.ConfigurationSCIMGroupMappingRoleOwner, contract.ConfigurationSCIMGroupMappingRoleOperator,
		contract.ConfigurationSCIMGroupMappingRoleDeveloper, contract.ConfigurationSCIMGroupMappingRoleViewer:
		return true
	}
	return false
}

func invalidGrant(p grant) bool {
	return access.ValidText("project", p.Project, 100) != nil || !p.Role.Valid()
}

func readSCIM(ctx context.Context, q access.Queryer) ([]group, error) {
	rows, err := q.Query(ctx, readGroups)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (group, error) {
		var name, scope string
		var role *string
		var projects []grant
		err := row.Scan(&name, &role, &scope, &projects)
		return group{
			Name:        name,
			Role:        promotion.Nullable((*contract.ConfigurationSCIMGroupMappingRole)(role)),
			AccessScope: contract.ConfigurationSCIMGroupMappingAccessScope(scope),
			Projects:    projects,
		}, err
	})
}

func renderSCIM(groups []group) []group {
	return slices.SortedFunc(slices.Values(mapped(groups, func(g group) group {
		return group{Name: g.Name, Role: g.Role, AccessScope: g.AccessScope, Projects: sortedGrants(g.Projects)}
	})), func(a, b group) int { return cmp.Compare(folded(a.Name), folded(b.Name)) })
}

func sortedGrants(grants []grant) []grant {
	return append([]grant{}, slices.SortedFunc(slices.Values(grants), func(a, b grant) int {
		return cmp.Compare(folded(a.Project), folded(b.Project))
	})...)
}

func planSCIM(ctx context.Context, q access.Queryer, d Desired) ([]promotion.Item, error) {
	if len(d.groups) == 0 {
		return nil, nil
	}
	current, err := readSCIM(ctx, q)
	if err != nil {
		return nil, err
	}
	directory, err := project.ReadDirectory(ctx, q)
	if err != nil {
		return nil, err
	}
	known := func(name string) bool {
		_, exists := directory.ID(name)
		_, declared := d.declared[folded(name)]
		return exists || declared
	}
	if slices.ContainsFunc(d.groups, func(g group) bool {
		return slices.ContainsFunc(g.Projects, func(p grant) bool { return !known(p.Project) })
	}) {
		return nil, access.Invalid("scim_group_mappings", missingProject)
	}
	existing := byName(current)
	return mapped(d.groups, func(g group) promotion.Item {
		stored, exists := existing[folded(g.Name)]
		return promotion.Item{
			Kind:   contract.ConfigurationPlanItemKindScimGroupMapping,
			Key:    g.Name,
			Action: action(exists, sameGroup(stored, g)),
			Detail: "Apply group access mapping; directory identities and memberships remain local.",
		}
	}), nil
}

func SCIMApplier(d Desired, deps Deps) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		if len(d.groups) == 0 {
			return nil
		}
		current, err := readSCIM(ctx, tx)
		if err != nil {
			return err
		}
		directory, err := project.ReadDirectory(ctx, tx)
		if err != nil {
			return err
		}
		existing := byName(current)
		return firstError(d.groups, func(g group) error {
			if stored, exists := existing[folded(g.Name)]; exists && sameGroup(stored, g) {
				return nil
			}
			if slices.ContainsFunc(g.Projects, func(p grant) bool { return lookup(directory, p.Project) == "" }) {
				return access.Invalid("scim_group_mappings", missingProject)
			}
			return deps.Access.StoreSCIMMapping(ctx, tx, p, g.Name, extension(g, directory))
		})
	}
}

func extension(g group, directory project.Directory) map[string]any {
	projects := mapped(g.Projects, func(p grant) any {
		return map[string]any{"value": lookup(directory, p.Project), "role": string(p.Role)}
	})
	role, ok := promotion.Present(g.Role)
	if !ok {
		return map[string]any{"accessScope": string(g.AccessScope), "projects": projects}
	}
	return map[string]any{"accessScope": string(g.AccessScope), "role": string(role), "projects": projects}
}

func sameGroup(a, b group) bool {
	roleA, hasA := promotion.Present(a.Role)
	roleB, hasB := promotion.Present(b.Role)
	return a.AccessScope == b.AccessScope && hasA == hasB && roleA == roleB &&
		slices.EqualFunc(sortedGrants(a.Projects), sortedGrants(b.Projects), func(x, y grant) bool {
			return folded(x.Project) == folded(y.Project) && x.Role == y.Role
		})
}

func byName(groups []group) map[string]group {
	return indexed(groups, func(g group) (string, group) { return folded(g.Name), g })
}
