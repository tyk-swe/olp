package installation

import (
	"context"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/settings"
)

const mfaDetail = "Change the local sign-in policy; factors, challenges and recovery codes remain installation-local."

func Planner(desired Settings) promotion.Planner {
	return promotion.Planner{Plan: func(ctx context.Context, q access.Queryer) ([]promotion.Item, error) {
		current, err := Read(ctx, q)
		if err != nil {
			return nil, err
		}
		return plan(desired, current), nil
	}}
}

func plan(desired, current Settings) []promotion.Item {
	return append(append(
		item(contract.ConfigurationPlanItemKindBudgetTimeZone, desired.TimeZone, current.TimeZone, ""),
		item(contract.ConfigurationPlanItemKindInstallationBudget, desired.Budget, current.Budget, "")...),
		item(contract.ConfigurationPlanItemKindRequireLocalMfa, desired.RequireLocalMFA, current.RequireLocalMFA, mfaDetail)...)
}

func item[T any](kind contract.ConfigurationPlanItemKind, desired, current *T, detail string) []promotion.Item {
	if desired == nil {
		return nil
	}
	if reflect.DeepEqual(desired, current) {
		return []promotion.Item{{Kind: kind, Key: string(kind), Action: contract.ConfigurationPlanItemActionReuse, Detail: detail}}
	}
	return []promotion.Item{{Kind: kind, Key: string(kind), Action: contract.ConfigurationPlanItemActionReplace, Detail: detail}}
}

func MFAApplier(desired Settings) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		if desired.RequireLocalMFA == nil {
			return nil
		}
		return access.SetLocalMFARequired(ctx, tx, p, *desired.RequireLocalMFA)
	}
}

func TimeZoneApplier(desired Settings) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		return settings.ApplyBudgetTimeZone(ctx, tx, p, desired.TimeZone)
	}
}

func BudgetApplier(desired Settings) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		changed, err := settings.ApplyInstallationBudget(ctx, tx, p, desired.Budget)
		if err != nil || !changed {
			return err
		}
		if _, err := access.AdvanceAuthority(ctx, tx); err != nil {
			return err
		}
		return nil
	}
}
