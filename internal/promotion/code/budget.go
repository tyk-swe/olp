package code

import (
	"context"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
)

type Budget struct {
	ID            string
	Project       string
	Route         *string
	APIKeyRef     *string
	DailyTokens   *int64
	MonthlyTokens *int64
	Enabled       bool
}

func parseBudget(e contract.ConfigurationCodeBudget, routes []Route) (Budget, error) {
	if access.ValidText("code", e.Project, 100) != nil {
		return Budget{}, access.Invalid("code.budgets.project", "Name the project.")
	}
	budget := Budget{ID: e.Id.String(), Project: e.Project, Route: text(promotion.Pointer(e.Route)), APIKeyRef: promotion.Pointer(e.ApiKeyRef), DailyTokens: promotion.Pointer(e.DailyTokens), MonthlyTokens: promotion.Pointer(e.MonthlyTokens), Enabled: e.Enabled}
	if budget.Route != nil && !slices.ContainsFunc(routes, func(r Route) bool { return r.ID == *budget.Route && strings.EqualFold(r.Project, budget.Project) }) {
		return Budget{}, access.Invalid("code.budgets.route", "The route must resolve within the budget project.")
	}
	return budget, budget.input(budget.ID, budget.Route, placeholderOf(budget.APIKeyRef, budget.ID)).Validate()
}

func text(id *openapi_types.UUID) *string {
	if id == nil {
		return nil
	}
	return new(id.String())
}

func (b Budget) input(project string, route, key *string) access.CodeBudgetInput {
	return access.CodeBudgetInput{ProjectID: project, RouteID: route, APIKeyID: key, DailyTokens: b.DailyTokens, MonthlyTokens: b.MonthlyTokens, Enabled: b.Enabled}
}

func (b Budget) keyRefs() []string {
	if b.APIKeyRef == nil {
		return []string{}
	}
	return []string{*b.APIKeyRef}
}

func readBudgets(ctx context.Context, q access.Queryer) ([]Budget, error) {
	budgets, err := access.ReadCodeBudgets(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.Map(budgets, func(b access.CodeBudget) Budget {
		return Budget{ID: b.ID, Project: b.Project, Route: b.RouteID, APIKeyRef: optionalRef("key", b.APIKeyID), DailyTokens: b.DailyTokens, MonthlyTokens: b.MonthlyTokens, Enabled: b.Enabled}
	}), nil
}

func renderBudget(b Budget) (contract.ConfigurationCodeBudget, error) {
	id, err := uuid.Parse(b.ID)
	if err != nil {
		return contract.ConfigurationCodeBudget{}, err
	}
	route, err := parsedOf(b.Route)
	if err != nil {
		return contract.ConfigurationCodeBudget{}, err
	}
	return contract.ConfigurationCodeBudget{Id: id, Project: b.Project, Route: promotion.Nullable(route), ApiKeyRef: promotion.Nullable(b.APIKeyRef), DailyTokens: promotion.Nullable(b.DailyTokens), MonthlyTokens: promotion.Nullable(b.MonthlyTokens), Enabled: b.Enabled}, nil
}

func parsedOf(value *string) (*openapi_types.UUID, error) {
	if value == nil {
		return nil, nil
	}
	id, err := uuid.Parse(*value)
	return &id, err
}

func resolveBudget(ctx context.Context, q access.Queryer, b Budget, desired Desired, directory promotion.Directory, routeIDs map[string]string) (resolved[access.CodeBudgetInput], error) {
	projectID, err := projectOf(directory, b.Project)
	if err != nil {
		return resolved[access.CodeBudgetInput]{}, err
	}
	keyID, err := optionalKey(ctx, q, b.APIKeyRef, projectID, desired.bindings.keys)
	if err != nil {
		return resolved[access.CodeBudgetInput]{}, err
	}
	routeID := destinationOf(b.Route, routeIDs)
	derived, err := derivedID("budget", projectID, b.ID)
	if err != nil {
		return resolved[access.CodeBudgetInput]{}, err
	}
	unique := !slices.ContainsFunc(desired.Code.Budgets, func(other Budget) bool {
		return other.ID != b.ID && strings.EqualFold(other.Project, b.Project) && boundOr(other.Route, routeIDs) == boundOr(b.Route, routeIDs) && boundOr(other.APIKeyRef, desired.bindings.keys) == boundOr(b.APIKeyRef, desired.bindings.keys) && (other.DailyTokens != nil) == (b.DailyTokens != nil) && (other.MonthlyTokens != nil) == (b.MonthlyTokens != nil)
	})
	candidates, err := access.CodeBudgetCandidates(ctx, q, b.ID, derived, access.CodeBudgetMatch{ProjectID: projectID, RouteID: routeID, APIKeyID: keyID, Daily: b.DailyTokens != nil, Monthly: b.MonthlyTokens != nil})
	if err != nil {
		return resolved[access.CodeBudgetInput]{}, err
	}
	to, err := mapped(b.ID, projectID, derived, unique, candidates)
	return resolved[access.CodeBudgetInput]{ref: b.ID, to: to, input: b.input(projectID, routeID, keyID)}, err
}

func optionalKey(ctx context.Context, q access.Queryer, ref *string, project string, keys map[string]string) (*string, error) {
	if ref == nil {
		return nil, nil
	}
	id, err := key(ctx, q, *ref, project, nil, keys)
	return &id, err
}

func destinationOf(ref *string, ids map[string]string) *string {
	if ref == nil {
		return nil
	}
	return new(ids[*ref])
}

func budgetChange(ctx context.Context, q access.Queryer, b resolved[access.CodeBudgetInput]) (change, error) {
	action, err := compared(ctx, q, b, access.ReadCodeBudgetInput)
	return change{
		item: promotion.Item{Kind: contract.ConfigurationPlanItemKindCodeBudget, Key: b.ref, Action: action},
		write: func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
			_, err := access.SaveCodeBudget(ctx, tx, p, b.to.id, access.NewID(), b.input, b.to.existing)
			return err
		},
	}, err
}
