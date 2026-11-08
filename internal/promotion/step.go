package promotion

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
)

type Planner struct {
	Claims    []Claim
	Authorize func(context.Context, access.Queryer, access.Principal) error
	Plan      func(context.Context, access.Queryer) ([]Item, error)
}

type Applier func(context.Context, pgx.Tx, access.Principal) error

type Names struct {
	projects, providers, organizations, routes map[string]bool
}

func NamesOf(document contract.ConfigurationDocument) Names {
	return Names{
		projects:      folded(slices.Values(document.Projects), func(entry contract.ConfigurationProjectEntry) string { return entry.Name }),
		providers:     folded(slices.Values(document.Providers), func(entry contract.ConfigurationProviderEntry) string { return entry.Name }),
		organizations: folded(slices.Values(valueOf(document.Organizations)), func(entry contract.ConfigurationOrganizationEntry) string { return entry.Name }),
		routes: maps.Collect(func(yield func(string, bool) bool) {
			for _, route := range document.Routes {
				if !yield(route.Slug, true) {
					return
				}
			}
		}),
	}
}

func (n Names) Project(name string) bool      { return n.projects[strings.ToLower(name)] }
func (n Names) Provider(name string) bool     { return n.providers[strings.ToLower(name)] }
func (n Names) Organization(name string) bool { return n.organizations[strings.ToLower(name)] }
func (n Names) Route(slug string) bool        { return n.routes[slug] }

func folded[T any](entries func(func(T) bool), name func(T) string) map[string]bool {
	return maps.Collect(func(yield func(string, bool) bool) {
		for entry := range entries {
			if !yield(strings.ToLower(name(entry)), true) {
				return
			}
		}
	})
}
