package project

import (
	"context"
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
		_, exists, err := projects.OrganizationIDByName(ctx, q, *project.Organization)
		if err != nil {
			return err
		}
		if !exists {
			return access.Invalid("organization", "Reference an organization in the document or destination.")
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
	return promotion.Map(desired, func(project Project) promotion.Item {
		return promotion.Item{Kind: contract.ConfigurationPlanItemKindProject, Key: project.Name, Action: action(project, current)}
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
	return promotion.Map(publishing(desired, current), func(project Project) promotion.Item {
		return promotion.Item{Kind: contract.ConfigurationPlanItemKindCatalogPublication, Key: project.Name, Action: contract.ConfigurationPlanItemActionReplace}
	})
}

func authorizeCatalog(p access.Principal, desired []Project, current State) error {
	if len(publishing(desired, current)) == 0 {
		return nil
	}
	return p.Authorize(access.Access)
}

func publishing(desired []Project, state State) []Project {
	return promotion.Filter(desired, func(project Project) bool {
		current, ok := state.Project(project.Name)
		if !ok {
			return project.PublicCatalog || project.PublicCatalogPrices
		}
		return current.Catalog.Enabled != project.PublicCatalog || current.Catalog.PricesPublic != project.PublicCatalogPrices
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
