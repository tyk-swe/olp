package organization

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
)

const maxOrganizations = 1000

type Organization struct {
	Name   string                  `json:"name"`
	Budget *authority.BudgetPolicy `json:"budget,omitempty"`
}

func Parse(document contract.ConfigurationDocument) ([]Organization, error) {
	if document.Organizations == nil {
		return nil, nil
	}
	if len(*document.Organizations) > maxOrganizations {
		return nil, access.Invalid("organizations", "Use at most 1000 organizations.")
	}
	declared, err := promotion.Convert[[]Organization](*document.Organizations)
	if err != nil {
		return nil, err
	}
	if err := validate(declared); err != nil {
		return nil, err
	}
	return slices.SortedFunc(func(yield func(Organization) bool) {
		for _, organization := range declared {
			if !yield(Organization{Name: strings.TrimSpace(organization.Name), Budget: authority.NormalizedBudget(organization.Budget)}) {
				return
			}
		}
	}, byName), nil
}

func Read(ctx context.Context, q access.Queryer) ([]Organization, error) {
	rows, err := q.Query(ctx, "SELECT name,budget_policy FROM olp.organizations")
	if err != nil {
		return nil, err
	}
	organizations, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Organization])
	if err != nil {
		return nil, err
	}
	return slices.SortedFunc(slices.Values(organizations), byName), nil
}

func Render(organizations []Organization) (*[]contract.ConfigurationOrganizationEntry, error) {
	if len(organizations) == 0 {
		return nil, nil
	}
	entries, err := promotion.Convert[[]contract.ConfigurationOrganizationEntry](organizations)
	if err != nil {
		return nil, err
	}
	return &entries, nil
}

func validate(organizations []Organization) error {
	first := firstIndex(organizations)
	for i, organization := range organizations {
		name := strings.TrimSpace(organization.Name)
		if err := access.ValidText("organizations", name, 100); err != nil {
			return err
		}
		if first[strings.ToLower(name)] != i {
			return access.Invalid("organizations", "Organization names must be unique.")
		}
		if err := organization.Budget.ValidateAndNormalize(); err != nil {
			return err
		}
	}
	return nil
}

func firstIndex(organizations []Organization) map[string]int {
	return maps.Collect(func(yield func(string, int) bool) {
		for i, organization := range slices.Backward(organizations) {
			if !yield(strings.ToLower(strings.TrimSpace(organization.Name)), i) {
				return
			}
		}
	})
}

func byName(a, b Organization) int {
	return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.Name, b.Name))
}
