package installation

import (
	"context"

	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/budgetcalendar"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/settings"
)

type Settings struct {
	TimeZone        *string
	Budget          *authority.BudgetPolicy
	RequireLocalMFA *bool
}

type Fragment struct {
	BudgetTimeZone     nullable.Nullable[string]
	InstallationBudget nullable.Nullable[contract.BudgetPolicy]
	RequireLocalMfa    nullable.Nullable[bool]
}

func Parse(document contract.ConfigurationDocument) (Settings, error) {
	zone, err := parseTimeZone(document.BudgetTimeZone)
	if err != nil {
		return Settings{}, err
	}
	budget, err := parseBudget(document.InstallationBudget)
	if err != nil {
		return Settings{}, err
	}
	return Settings{TimeZone: zone, Budget: budget, RequireLocalMFA: promotion.Pointer(document.RequireLocalMfa)}, nil
}

func Read(ctx context.Context, q access.Queryer) (Settings, error) {
	zone, err := settings.ReadBudgetTimeZone(ctx, q)
	if err != nil {
		return Settings{}, err
	}
	budget, err := settings.ReadInstallationBudget(ctx, q)
	if err != nil {
		return Settings{}, err
	}
	required, err := access.ReadLocalMFARequired(ctx, q)
	if err != nil {
		return Settings{}, err
	}
	return Settings{TimeZone: &zone, Budget: canonical(budget), RequireLocalMFA: &required}, nil
}

func Render(current Settings) (Fragment, error) {
	budget, err := renderBudget(current.Budget)
	if err != nil {
		return Fragment{}, err
	}
	return Fragment{
		BudgetTimeZone:     promotion.Nullable(current.TimeZone),
		InstallationBudget: budget,
		RequireLocalMfa:    promotion.Nullable(current.RequireLocalMFA),
	}, nil
}

func parseTimeZone(value nullable.Nullable[string]) (*string, error) {
	zone := promotion.Pointer(value)
	if zone == nil {
		return nil, nil
	}
	if _, err := budgetcalendar.New(*zone); err != nil {
		return nil, access.Invalid("budget_time_zone", "Use a supported IANA time zone.")
	}
	return zone, nil
}

func parseBudget(value nullable.Nullable[contract.BudgetPolicy]) (*authority.BudgetPolicy, error) {
	budget, ok := promotion.Present(value)
	if !ok {
		return nil, nil
	}
	policy, err := promotion.Convert[authority.BudgetPolicy](budget)
	if err != nil {
		return nil, err
	}
	if err := policy.ValidateAndNormalize(); err != nil {
		return nil, err
	}
	return canonical(&policy), nil
}

func renderBudget(policy *authority.BudgetPolicy) (nullable.Nullable[contract.BudgetPolicy], error) {
	if policy == nil {
		return nullable.NewNullNullable[contract.BudgetPolicy](), nil
	}
	budget, err := promotion.Convert[contract.BudgetPolicy](policy)
	if err != nil {
		return nil, err
	}
	return nullable.NewNullableWithValue(budget), nil
}

func canonical(policy *authority.BudgetPolicy) *authority.BudgetPolicy {
	if normalized := authority.NormalizedBudget(policy); normalized != nil {
		return normalized
	}
	return &authority.BudgetPolicy{}
}
