package route

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion"
)

type group struct {
	project string
	members []string
}

func (g group) excludes(r Route) bool {
	return slices.Contains(g.members, r.Slug) && (r.Project == nil || !strings.EqualFold(*r.Project, g.project))
}

func invalidGroup() error {
	return access.Invalid("route_groups", "Group routes must belong to their project.")
}

func parseGroups(entries []contract.ConfigurationProjectEntry, declared []Route) ([]group, error) {
	groups := slices.Collect(func(yield func(group) bool) {
		for _, entry := range entries {
			assigned, _ := promotion.Present(entry.RouteGroups)
			for _, members := range assigned {
				if !yield(group{project: strings.TrimSpace(entry.Name), members: members}) {
					return
				}
			}
		}
	})
	if slices.ContainsFunc(groups, func(g group) bool { return slices.ContainsFunc(declared, g.excludes) }) {
		return nil, invalidGroup()
	}
	return groups, nil
}

func checkGroups(ctx context.Context, q access.Queryer, groups []group) error {
	return every(groups, func(_ int, g group) error {
		var foreign bool
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM (SELECT slug,project_id FROM olp.routes UNION ALL SELECT slug,project_id FROM olp.code_routes) r
			LEFT JOIN olp.projects p ON p.id=r.project_id WHERE r.slug=ANY($1::text[]) AND lower(p.name) IS DISTINCT FROM lower($2::text))`, g.members, g.project).Scan(&foreign); err != nil {
			return err
		}
		if foreign {
			return invalidGroup()
		}
		return nil
	})
}

func GroupApplier(desired Desired) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, _ access.Principal) error {
		return checkGroups(ctx, tx, desired.groups)
	}
}

func ReferenceApplier(desired Desired) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, _ access.Principal) error {
		return every(desired.projects, func(_ int, name string) error {
			var id string
			var templates keyauthority.LimitTemplates
			if err := tx.QueryRow(ctx, "SELECT id::text,limit_templates FROM olp.projects WHERE lower(name)=lower($1)", name).Scan(&id, &templates); err != nil {
				return err
			}
			return projects.ValidateTemplateReferences(ctx, tx, id, templates)
		})
	}
}
