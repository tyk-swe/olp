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
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/platform/database"
)

type Planner struct {
	Claims    []Claim
	Authorize func(context.Context, database.Queryer, authz.Principal) error
	Plan      func(context.Context, database.Queryer) ([]Item, error)
}

type Applier func(context.Context, pgx.Tx, authz.Principal) error

type Names struct {
	projects, providers, organizations map[string]bool
}

func NamesOf(document contract.ConfigurationDocument) Names {
	return Names{
		projects:      folded(slices.Values(document.Projects), func(entry contract.ConfigurationProjectEntry) string { return entry.Name }),
		providers:     folded(slices.Values(document.Providers), func(entry contract.ConfigurationProviderEntry) string { return entry.Name }),
		organizations: folded(slices.Values(ptr.Value(document.Organizations)), func(entry contract.ConfigurationOrganizationEntry) string { return entry.Name }),
	}
}

func (n Names) Project(name string) bool      { return n.projects[Fold(name)] }
func (n Names) Provider(name string) bool     { return n.providers[Fold(name)] }
func (n Names) Organization(name string) bool { return n.organizations[Fold(name)] }

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
	if known, ok := errors.AsType[*httpkit.Problem](err); ok && known.Field != "" {
		return &httpkit.Problem{Status: known.Status, Code: known.Code, Detail: known.Detail, Field: prefix + "." + known.Field}
	}
	return err
}

func UnknownProject(name string) error {
	return httpkit.Fail(http.StatusConflict, "project_unknown", "Project "+name+" is not in the destination.")
}

func UnknownOrganization(name string) error {
	return httpkit.Fail(http.StatusConflict, "organization_unknown", "Organization "+name+" is not in the destination.")
}

func UnknownProvider(name string) error {
	return httpkit.Fail(http.StatusConflict, "provider_unknown", "Provider "+name+" is not in the destination.")
}
