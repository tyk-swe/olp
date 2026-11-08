package project

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/modelcatalog"
	"github.com/tyk-swe/olp/internal/projects"
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
	rows, err := q.Query(ctx, `SELECT id::text,name,(SELECT name FROM olp.organizations WHERE id=organization_id),end_user_policy,attribution_policy,route_groups,budget_policy,limit_templates,attribution_budgets FROM olp.projects`)
	if err != nil {
		return State{}, err
	}
	scanned, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Current, error) {
		var current Current
		err := row.Scan(&current.ID, &current.Name, &current.Organization, &current.Policies.EndUser, &current.Policies.Attribution, &current.Policies.RouteGroups, &current.Policies.Budget, &current.Policies.Templates, &current.Policies.AttributionBudgets)
		return current, err
	})
	if err != nil {
		return State{}, err
	}
	publications, err := modelcatalog.ReadPublications(ctx, q)
	if err != nil {
		return State{}, err
	}
	return stateOf(slices.SortedFunc(func(yield func(Current) bool) {
		for _, current := range scanned {
			if !yield(Current{ID: current.ID, Name: current.Name, Organization: current.Organization, Policies: current.Policies, Catalog: publications[current.ID]}) {
				return
			}
		}
	}, func(a, b Current) int { return byName(Project{Name: a.Name}, Project{Name: b.Name}) })), nil
}

func stateOf(current []Current) State {
	return State{projects: current, byName: maps.Collect(func(yield func(string, Current) bool) {
		for _, project := range current {
			if !yield(strings.ToLower(project.Name), project) {
				return
			}
		}
	})}
}

func (s State) Project(name string) (Current, bool) {
	current, ok := s.byName[strings.ToLower(name)]
	return current, ok
}

func (s State) Projects() []Project {
	return slices.Collect(func(yield func(Project) bool) {
		for _, current := range s.projects {
			if !yield(current.exported()) {
				return
			}
		}
	})
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
