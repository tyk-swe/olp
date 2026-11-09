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
)

type grant = struct {
	Project string                                             `json:"project"`
	Role    contract.ConfigurationSCIMGroupMappingProjectsRole `json:"role"`
}

type group = contract.ConfigurationSCIMGroupMapping

const (
	missingProject = "Every mapped project must exist or be declared."
	invalidName    = "Use unique nonempty group names up to 100 characters."
)

func parseSCIM(fragment nullable.Nullable[[]group]) ([]group, error) {
	entries := fragment.GetOrEmpty()
	if len(entries) > 1000 {
		return nil, access.Invalid("scim_group_mappings", "At most 1000 group mappings are supported.")
	}
	if _, repeated := promotion.FirstRepeat(entries, func(g group) string { return promotion.Fold(g.Name) }); repeated {
		return nil, access.Invalid("scim_group_mappings", invalidName)
	}
	if err := promotion.Every(entries, validGroup); err != nil {
		return nil, err
	}
	return promotion.Map(entries, func(g group) group {
		return group{Name: g.Name, Role: g.Role, Projects: g.Projects, AccessScope: cmp.Or(g.AccessScope, contract.ConfigurationSCIMGroupMappingAccessScopeAssigned)}
	}), nil
}

func validGroup(g group) error {
	role, hasRole := promotion.Present(g.Role)
	_, repeated := promotion.FirstRepeat(g.Projects, func(p grant) string { return promotion.Fold(p.Project) })
	switch {
	case access.ValidText("name", g.Name, 100) != nil:
		return access.Invalid("scim_group_mappings", invalidName)
	case g.AccessScope != "" && !g.AccessScope.Valid():
		return access.Invalid("scim_group_mappings", "Use assigned or global scope.")
	case hasRole && !installationRole(role):
		return access.Invalid("scim_group_mappings", "Unknown installation role.")
	case len(g.Projects) > 100:
		return access.Invalid("scim_group_mappings", "At most 100 project grants per group.")
	case repeated || slices.ContainsFunc(g.Projects, invalidGrant):
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
	stored, err := access.ReadSCIMGroupMappings(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.Map(stored, func(mapping access.SCIMGroupMapping) group {
		return group{
			Name:        mapping.Name,
			Role:        promotion.Nullable((*contract.ConfigurationSCIMGroupMappingRole)(mapping.Role)),
			AccessScope: contract.ConfigurationSCIMGroupMappingAccessScope(mapping.AccessScope),
			Projects: promotion.Map(mapping.Projects, func(g access.SCIMProjectGrant) grant {
				return grant{Project: g.Project, Role: contract.ConfigurationSCIMGroupMappingProjectsRole(g.Role)}
			}),
		}
	}), nil
}

func renderSCIM(groups []group) []group {
	return slices.SortedFunc(slices.Values(promotion.Map(groups, func(g group) group {
		return group{Name: g.Name, Role: g.Role, AccessScope: g.AccessScope, Projects: sortedGrants(g.Projects)}
	})), func(a, b group) int { return cmp.Compare(promotion.Fold(a.Name), promotion.Fold(b.Name)) })
}

func sortedGrants(grants []grant) []grant {
	return promotion.NonNil(slices.SortedFunc(slices.Values(grants), func(a, b grant) int {
		return cmp.Compare(promotion.Fold(a.Project), promotion.Fold(b.Project))
	}))
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
		_, declared := d.declared[promotion.Fold(name)]
		return exists || declared
	}
	if slices.ContainsFunc(d.groups, func(g group) bool {
		return slices.ContainsFunc(g.Projects, func(p grant) bool { return !known(p.Project) })
	}) {
		return nil, access.Invalid("scim_group_mappings", missingProject)
	}
	existing := byName(current)
	return promotion.Map(d.groups, func(g group) promotion.Item {
		stored, exists := existing[promotion.Fold(g.Name)]
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
		return promotion.Every(d.groups, func(g group) error {
			if stored, exists := existing[promotion.Fold(g.Name)]; exists && sameGroup(stored, g) {
				return nil
			}
			mapping, err := extension(g, directory)
			if err != nil {
				return err
			}
			return deps.Access.StoreSCIMMapping(ctx, tx, p, g.Name, mapping)
		})
	}
}

func extension(g group, directory promotion.Directory) (map[string]any, error) {
	projects, err := promotion.TryMap(g.Projects, func(p grant) (any, error) {
		id, ok := directory.ID(p.Project)
		if !ok {
			return nil, access.Invalid("scim_group_mappings", missingProject)
		}
		return map[string]any{"value": id, "role": string(p.Role)}, nil
	})
	if err != nil {
		return nil, err
	}
	role, ok := promotion.Present(g.Role)
	if !ok {
		return map[string]any{"accessScope": string(g.AccessScope), "projects": projects}, nil
	}
	return map[string]any{"accessScope": string(g.AccessScope), "role": string(role), "projects": projects}, nil
}

func sameGroup(a, b group) bool {
	roleA, hasA := promotion.Present(a.Role)
	roleB, hasB := promotion.Present(b.Role)
	return a.AccessScope == b.AccessScope && hasA == hasB && roleA == roleB &&
		slices.EqualFunc(sortedGrants(a.Projects), sortedGrants(b.Projects), func(x, y grant) bool {
			return promotion.Fold(x.Project) == promotion.Fold(y.Project) && x.Role == y.Role
		})
}

func byName(groups []group) map[string]group {
	return promotion.Associate(groups, func(g group) (string, group) { return promotion.Fold(g.Name), g })
}
