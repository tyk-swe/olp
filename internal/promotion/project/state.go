package project

import (
	"context"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/modelcatalog"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion"
)

type Current struct {
	ID           string
	Name         string
	Organization *string
	Policies     projects.Policies
	Catalog      modelcatalog.Publication
}

type State struct {
	projects []Current
	byName   map[string]Current
}

func Read(ctx context.Context, q access.Queryer) (State, error) {
	stored, err := projects.ReadProjectPolicies(ctx, q)
	if err != nil {
		return State{}, err
	}
	publications, err := modelcatalog.ReadPublications(ctx, q)
	if err != nil {
		return State{}, err
	}
	current := promotion.Map(stored, func(project projects.ProjectPolicies) Current {
		return Current{ID: project.ID, Name: project.Name, Organization: project.Organization, Policies: project.Policies, Catalog: publications[project.ID]}
	})
	sorted := slices.SortedFunc(slices.Values(current), func(a, b Current) int { return byName(Project{Name: a.Name}, Project{Name: b.Name}) })
	return State{projects: sorted, byName: promotion.Index(sorted, func(project Current) string { return strings.ToLower(project.Name) })}, nil
}

func (s State) Project(name string) (Current, bool) {
	current, ok := s.byName[strings.ToLower(name)]
	return current, ok
}

func (s State) Projects() []Project {
	return promotion.Map(s.projects, Current.exported)
}

func (c Current) exported() Project {
	return Project{
		Name:                c.Name,
		Organization:        c.Organization,
		EndUserDefaults:     defaults(c.Policies.EndUser),
		EndUserTemplate:     template(c.Policies.EndUser),
		Attribution:         c.Policies.Attribution,
		RouteGroups:         sortedGroups(c.Policies.RouteGroups),
		Budget:              c.Policies.Budget,
		Templates:           templates(c.Policies.Templates),
		AttributionBudgets:  c.Policies.AttributionBudgets,
		PublicCatalog:       c.Catalog.Enabled,
		PublicCatalogPrices: c.Catalog.PricesPublic,
	}
}

func defaults(policy *authority.EndUserPolicy) *authority.AdmissionLimits {
	if policy == nil {
		return nil
	}
	return &policy.Defaults
}

func template(policy *authority.EndUserPolicy) *string {
	if policy == nil {
		return nil
	}
	return policy.LimitTemplate
}

func templates(current authority.LimitTemplates) *authority.LimitTemplates {
	if len(current) == 0 {
		return nil
	}
	return &current
}
