package organization

import (
	"context"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion"
)

const detail = "Organization budget; destination memberships are preserved."

func Planner(desired []Organization) promotion.Planner {
	return promotion.Planner{Plan: func(ctx context.Context, q access.Queryer) ([]promotion.Item, error) {
		current, err := Read(ctx, q)
		if err != nil {
			return nil, err
		}
		return plan(desired, current), nil
	}}
}

func plan(desired, current []Organization) []promotion.Item {
	existing := promotion.Index(current, func(organization Organization) string { return strings.ToLower(organization.Name) })
	return promotion.Map(desired, func(organization Organization) promotion.Item {
		return promotion.Item{Kind: contract.ConfigurationPlanItemKindOrganization, Key: organization.Name, Action: action(organization, existing), Detail: detail}
	})
}

func action(desired Organization, existing map[string]Organization) contract.ConfigurationPlanItemAction {
	current, ok := existing[strings.ToLower(desired.Name)]
	switch {
	case !ok:
		return contract.ConfigurationPlanItemActionCreate
	case reflect.DeepEqual(current.Budget, desired.Budget):
		return contract.ConfigurationPlanItemActionReuse
	default:
		return contract.ConfigurationPlanItemActionReplace
	}
}

func Applier(desired []Organization) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		changed, err := applyAll(ctx, tx, p, desired)
		if err != nil || !changed {
			return err
		}
		if _, err := access.AdvanceAuthority(ctx, tx); err != nil {
			return err
		}
		return nil
	}
}

func applyAll(ctx context.Context, tx pgx.Tx, p access.Principal, organizations []Organization) (bool, error) {
	if len(organizations) == 0 {
		return false, nil
	}
	changed, err := apply(ctx, tx, p, organizations[0])
	if err != nil {
		return false, err
	}
	rest, err := applyAll(ctx, tx, p, organizations[1:])
	return changed || rest, err
}

func apply(ctx context.Context, tx pgx.Tx, p access.Principal, organization Organization) (bool, error) {
	current, found, err := projects.LockOrganizationByName(ctx, tx, organization.Name)
	if err != nil {
		return false, err
	}
	if !found {
		return create(ctx, tx, p, organization)
	}
	return setBudget(ctx, tx, p, current, organization.Budget)
}

func create(ctx context.Context, tx pgx.Tx, p access.Principal, organization Organization) (bool, error) {
	if err := p.Authorize(access.Access); err != nil {
		return false, err
	}
	id, _, err := projects.CreateOrganization(ctx, tx, organization.Name, p.UserID())
	if err != nil {
		return false, err
	}
	if _, err := setBudget(ctx, tx, p, projects.LockedOrganization{ID: id}, organization.Budget); err != nil {
		return false, err
	}
	return true, nil
}

func setBudget(ctx context.Context, tx pgx.Tx, p access.Principal, current projects.LockedOrganization, desired *authority.BudgetPolicy) (bool, error) {
	if reflect.DeepEqual(current.Budget, desired) {
		return false, nil
	}
	if err := p.Authorize(access.ManageOrganization); err != nil {
		return false, err
	}
	if err := p.Organization(current.ID, access.Change); err != nil {
		return false, err
	}
	if _, err := projects.SetOrganizationBudget(ctx, tx, current.ID, desired); err != nil {
		return false, err
	}
	return true, nil
}
