package route

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/routes"
)

type group struct {
	project string
	members []string
}

func groupsOf(entries []contract.ConfigurationProjectEntry) []group {
	return slices.Concat(promotion.Map(entries, func(entry contract.ConfigurationProjectEntry) []group {
		return slices.Collect(func(yield func(group) bool) {
			for _, members := range entry.RouteGroups.GetOrEmpty() {
				if !yield(group{project: strings.TrimSpace(entry.Name), members: members}) {
					return
				}
			}
		})
	})...)
}

func checkGroups(ctx context.Context, q access.Queryer, directory promotion.Directory, groups []group) error {
	return promotion.Every(groups, func(g group) error {
		owners, err := routes.SlugProjectIDs(ctx, q, g.members)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(owners, func(id *string) bool { return !ownedBy(directory, id, g.project) }) {
			return access.Invalid("route_groups", "Group routes must belong to their project.")
		}
		return nil
	})
}

func ownedBy(directory promotion.Directory, projectID *string, name string) bool {
	owner := projectName(directory, projectID)
	return owner != nil && strings.EqualFold(*owner, name)
}

func GroupApplier(desired Desired) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, _ access.Principal) error {
		directory, err := project.ReadDirectory(ctx, tx)
		if err != nil {
			return err
		}
		return checkGroups(ctx, tx, directory, desired.groups)
	}
}
