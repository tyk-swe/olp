package project

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/attribution"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/domain/textfield"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
)

const maxProjects = 100

type Project struct {
	Name                string                          `json:"name"`
	Organization        *string                         `json:"organization,omitempty"`
	EndUserDefaults     *keyauthority.AdmissionLimits   `json:"end_user_defaults,omitempty"`
	EndUserTemplate     *string                         `json:"end_user_limit_template,omitempty"`
	Attribution         *attribution.Policy             `json:"attribution_policy,omitempty"`
	RouteGroups         keyauthority.RouteGroups        `json:"route_groups,omitempty"`
	Budget              *keyauthority.BudgetPolicy      `json:"budget,omitempty"`
	Templates           *keyauthority.LimitTemplates    `json:"limit_templates,omitempty"`
	AttributionBudgets  keyauthority.AttributionBudgets `json:"attribution_budgets,omitempty"`
	PublicCatalog       bool                            `json:"public_catalog,omitempty"`
	PublicCatalogPrices bool                            `json:"public_catalog_prices,omitempty"`
}

func Parse(document contract.ConfigurationDocument) ([]Project, error) {
	if len(document.Projects) > maxProjects {
		return nil, problem.Invalid("projects", "Declare at most 100 projects.")
	}
	declared, err := promotion.Convert[[]Project](document.Projects)
	if err != nil {
		return nil, err
	}
	normalized := promotion.Map(declared, Project.normalized)
	if err := validate(normalized, document.Routes); err != nil {
		return nil, err
	}
	return slices.SortedFunc(slices.Values(normalized), byName), nil
}

func Render(projects []Project) ([]contract.ConfigurationProjectEntry, error) {
	return promotion.Convert[[]contract.ConfigurationProjectEntry](ptr.NonNil(slices.SortedFunc(slices.Values(projects), byName)))
}

func (p Project) normalized() Project {
	return Project{
		Name:                strings.TrimSpace(p.Name),
		Organization:        promotion.Trimmed(p.Organization),
		EndUserDefaults:     p.EndUserDefaults,
		EndUserTemplate:     p.EndUserTemplate,
		Attribution:         p.Attribution,
		RouteGroups:         sortedGroups(p.RouteGroups),
		Budget:              keyauthority.NormalizedBudget(p.Budget),
		Templates:           p.Templates,
		AttributionBudgets:  p.AttributionBudgets,
		PublicCatalog:       p.PublicCatalog,
		PublicCatalogPrices: p.PublicCatalogPrices,
	}
}

func validate(projects []Project, routes []contract.ConfigurationRouteEntry) error {
	first := promotion.FirstIndex(projects, func(p Project) string { return strings.ToLower(p.Name) })
	for i, project := range projects {
		if err := project.validate(routes); err != nil {
			return err
		}
		field := "projects." + strconv.Itoa(i) + ".name"
		if err := textfield.Validate(field, project.Name, 100); err != nil {
			return err
		}
		if first[strings.ToLower(project.Name)] != i {
			return problem.Invalid(field, "Project names must be unique.")
		}
	}
	return nil
}

func (p Project) validate(routes []contract.ConfigurationRouteEntry) error {
	if err := p.Budget.ValidateAndNormalize(); err != nil {
		return err
	}
	if err := p.AttributionBudgets.ValidateAndNormalize(); err != nil {
		return err
	}
	if p.Templates != nil {
		if err := p.Templates.ValidateAndNormalize(); err != nil {
			return err
		}
	}
	if p.EndUserTemplate != nil && !routeidentity.ValidSlug(*p.EndUserTemplate) {
		return problem.Invalid("end_user_limit_template", "Use a valid template name.")
	}
	if err := p.RouteGroups.Validate(); err != nil {
		return err
	}
	if slices.ContainsFunc(routes, p.foreignMember) {
		return foreignRoutes()
	}
	if p.Attribution != nil && p.Attribution.Validate() != nil {
		return problem.Invalid("projects.attribution_policy", "Use at most four distinct valid attribution labels.")
	}
	if p.EndUserDefaults != nil {
		return p.EndUserDefaults.ValidateAndNormalize()
	}
	return nil
}

func (p Project) foreignMember(route contract.ConfigurationRouteEntry) bool {
	owner, owned := promotion.Present(route.Project)
	return (!owned || !strings.EqualFold(strings.TrimSpace(owner), p.Name)) && p.groups(strings.TrimSpace(route.Slug))
}

func (p Project) groups(slug string) bool {
	for _, members := range p.RouteGroups {
		if slices.Contains(members, slug) {
			return true
		}
	}
	return false
}

func sortedGroups(groups keyauthority.RouteGroups) keyauthority.RouteGroups {
	if groups == nil {
		return nil
	}
	return maps.Collect(func(yield func(string, []string) bool) {
		for name, members := range groups {
			if !yield(name, sortedMembers(members)) {
				return
			}
		}
	})
}

func sortedMembers(members []string) []string {
	if members == nil {
		return nil
	}
	return ptr.NonNil(slices.Sorted(slices.Values(members)))
}

func byName(a, b Project) int {
	return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.Name, b.Name))
}
