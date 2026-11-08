package route

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
)

type group struct {
	project string
	members []string
}

func groupsOf(entries []contract.ConfigurationProjectEntry) []group {
	return slices.Concat(promotion.Map(entries, func(entry contract.ConfigurationProjectEntry) []group {
		assigned, _ := promotion.Present(entry.RouteGroups)
		return slices.Collect(func(yield func(group) bool) {
			for _, members := range assigned {
				if !yield(group{project: strings.TrimSpace(entry.Name), members: members}) {
					return
				}
			}
		})
	})...)
}

func checkGroups(ctx context.Context, q access.Queryer, groups []group) error {
	return promotion.Every(groups, func(g group) error {
		var foreign bool
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM (SELECT slug,project_id FROM olp.routes UNION ALL SELECT slug,project_id FROM olp.code_routes) r
			LEFT JOIN olp.projects p ON p.id=r.project_id WHERE r.slug=ANY($1::text[]) AND lower(p.name) IS DISTINCT FROM lower($2::text))`, g.members, g.project).Scan(&foreign); err != nil {
			return err
		}
		if foreign {
			return access.Invalid("route_groups", "Group routes must belong to their project.")
		}
		return nil
	})
}

func GroupApplier(desired Desired) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, _ access.Principal) error {
		return checkGroups(ctx, tx, desired.groups)
	}
}
