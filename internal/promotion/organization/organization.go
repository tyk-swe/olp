package organization

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/projects"
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
	return slices.SortedFunc(slices.Values(promotion.Map(declared, func(organization Organization) Organization {
		return Organization{Name: strings.TrimSpace(organization.Name), Budget: authority.NormalizedBudget(organization.Budget)}
	})), byName), nil
}

func Read(ctx context.Context, q access.Queryer) ([]Organization, error) {
	stored, err := projects.ReadOrganizationBudgets(ctx, q)
	if err != nil {
		return nil, err
	}
	organizations := promotion.Map(stored, func(organization projects.OrganizationBudget) Organization {
		return Organization{Name: organization.Name, Budget: organization.Budget}
	})
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
	first := promotion.FirstIndex(organizations, func(organization Organization) string {
		return strings.ToLower(strings.TrimSpace(organization.Name))
	})
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

func byName(a, b Organization) int {
	return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.Name, b.Name))
}
