package promotion

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
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
		organizations: folded(slices.Values(ValueOf(document.Organizations)), func(entry contract.ConfigurationOrganizationEntry) string { return entry.Name }),
		routes: maps.Collect(func(yield func(string, bool) bool) {
			for _, route := range document.Routes {
				if !yield(route.Slug, true) {
					return
				}
			}
		}),
	}
}

func (n Names) Project(name string) bool      { return n.projects[Fold(name)] }
func (n Names) Provider(name string) bool     { return n.providers[Fold(name)] }
func (n Names) Organization(name string) bool { return n.organizations[Fold(name)] }
func (n Names) Route(slug string) bool        { return n.routes[slug] }

func Fold(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

func Trimmed(value *string) *string {
	if value == nil {
		return nil
	}
	return new(strings.TrimSpace(*value))
}

func folded[T any](entries func(func(T) bool), name func(T) string) map[string]bool {
	return maps.Collect(func(yield func(string, bool) bool) {
		for entry := range entries {
			if !yield(Fold(name(entry)), true) {
				return
			}
		}
	})
}

func ValidLabel(value string) bool {
	length := utf8.RuneCountInString(value)
	return utf8.ValidString(value) && length >= 1 && length <= 100 && !strings.ContainsFunc(value, unicode.IsControl)
}

func WithFieldPrefix(err error, prefix string) error {
	if validation, ok := errors.AsType[*keyauthority.ValidationError](err); ok {
		return &keyauthority.ValidationError{Field: prefix + "." + validation.Field, Detail: validation.Detail}
	}
	if problem, ok := errors.AsType[*access.Problem](err); ok && problem.Field != "" {
		return &access.Problem{Status: problem.Status, Code: problem.Code, Detail: problem.Detail, Field: prefix + "." + problem.Field}
	}
	return err
}

func UnknownProject(name string) error {
	return access.Fail(http.StatusConflict, "project_unknown", "Project "+name+" is not in the destination.")
}

func UnknownOrganization(name string) error {
	return access.Fail(http.StatusConflict, "organization_unknown", "Organization "+name+" is not in the destination.")
}

func UnknownProvider(name string) error {
	return access.Fail(http.StatusConflict, "provider_unknown", "Provider "+name+" is not in the destination.")
}
