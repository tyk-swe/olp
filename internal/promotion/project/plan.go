package project

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion"
)

func Planner(desired []Project, names promotion.Names) promotion.Planner {
	return promotion.Planner{
		Authorize: func(ctx context.Context, q access.Queryer, p access.Principal) error {
			current, err := Read(ctx, q)
			if err != nil {
				return err
			}
			return authorizeCatalog(p, desired, current)
		},
		Plan: func(ctx context.Context, q access.Queryer) ([]promotion.Item, error) {
			if err := checkOrganizations(ctx, q, desired, names); err != nil {
				return nil, err
			}
			if err := checkRouteGroups(ctx, q, desired); err != nil {
				return nil, err
			}
			current, err := Read(ctx, q)
			if err != nil {
				return nil, err
			}
			if err := checkMoves(desired, current); err != nil {
				return nil, err
			}
			return slices.Concat(projectItems(desired, current), catalogItems(desired, current)), nil
		},
	}
}

func checkOrganizations(ctx context.Context, q access.Queryer, desired []Project, names promotion.Names) error {
	for _, project := range desired {
		if project.Organization == nil || names.Organization(*project.Organization) {
			continue
		}
		var exists bool
		if err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.organizations WHERE lower(name)=lower($1))", *project.Organization).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return access.Invalid("organization", "Reference an organization in the document or destination.")
		}
	}
	return nil
}

func checkRouteGroups(ctx context.Context, q access.Queryer, desired []Project) error {
	for _, project := range desired {
		for _, group := range slices.Sorted(maps.Keys(project.RouteGroups)) {
			var foreign bool
			if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM (SELECT slug,project_id FROM olp.routes UNION ALL SELECT slug,project_id FROM olp.code_routes) r LEFT JOIN olp.projects p ON p.id=r.project_id WHERE r.slug=ANY($1::text[]) AND lower(p.name) IS DISTINCT FROM lower($2::text))`, project.RouteGroups[group], project.Name).Scan(&foreign); err != nil {
				return err
			}
			if foreign {
				return foreignRoutes()
			}
		}
	}
	return nil
}

func checkMoves(desired []Project, current State) error {
	for _, project := range desired {
		existing, ok := current.Project(project.Name)
		if ok && moves(project, existing) {
			return organizationImmutable()
		}
	}
	return nil
}

func moves(desired Project, current Current) bool {
	return desired.Organization != nil && current.Organization != nil && !strings.EqualFold(*desired.Organization, *current.Organization)
}

func projectItems(desired []Project, current State) []promotion.Item {
	return slices.Collect(func(yield func(promotion.Item) bool) {
		for _, project := range desired {
			if !yield(promotion.Item{Kind: contract.ConfigurationPlanItemKindProject, Key: project.Name, Action: action(project, current)}) {
				return
			}
		}
	})
}

func action(desired Project, state State) contract.ConfigurationPlanItemAction {
	current, ok := state.Project(desired.Name)
	switch {
	case !ok:
		return contract.ConfigurationPlanItemActionCreate
	case desired.Organization != nil && current.Organization == nil, desired.imported(current.ID).Changes(current.Policies):
		return contract.ConfigurationPlanItemActionReplace
	default:
		return contract.ConfigurationPlanItemActionReuse
	}
}

func catalogItems(desired []Project, current State) []promotion.Item {
	return slices.Collect(func(yield func(promotion.Item) bool) {
		for _, project := range publishing(desired, current) {
			if !yield(promotion.Item{Kind: contract.ConfigurationPlanItemKindCatalogPublication, Key: project.Name, Action: contract.ConfigurationPlanItemActionReplace}) {
				return
			}
		}
	})
}

func authorizeCatalog(p access.Principal, desired []Project, current State) error {
	if len(publishing(desired, current)) == 0 {
		return nil
	}
	return p.Authorize(access.Access)
}

func publishing(desired []Project, state State) []Project {
	return slices.Collect(func(yield func(Project) bool) {
		for _, project := range desired {
			current, _ := state.Project(project.Name)
			if (current.Catalog.Enabled != project.PublicCatalog || current.Catalog.PricesPublic != project.PublicCatalogPrices) && !yield(project) {
				return
			}
		}
	})
}

func (p Project) imported(id string) projects.PolicyImport {
	return projects.PolicyImport{
		ProjectID:          id,
		EndUserDefaults:    p.EndUserDefaults,
		EndUserTemplate:    p.EndUserTemplate,
		Attribution:        p.Attribution,
		RouteGroups:        p.RouteGroups,
		Budget:             p.Budget,
		Templates:          p.Templates,
		AttributionBudgets: p.AttributionBudgets,
	}
}

func foreignRoutes() error {
	return access.Invalid("route_groups", "Group routes must belong to their project.")
}

func organizationImmutable() error {
	return access.Fail(http.StatusConflict, "project_organization_immutable", "A project cannot move between organizations.")
}
