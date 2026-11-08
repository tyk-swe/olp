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
	"github.com/tyk-swe/olp/internal/promotion/project"
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
	return promotion.Rows(ctx, q, pgx.RowToStructByPos[Budget], `SELECT x.id::text,p.name,x.route_id::text,CASE WHEN x.api_key_id IS NULL THEN NULL ELSE 'code/key/'||x.api_key_id END,x.daily_tokens,x.monthly_tokens,x.enabled FROM olp.code_token_budgets x JOIN olp.projects p ON p.id=x.project_id`)
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

func resolveBudget(ctx context.Context, q access.Queryer, b Budget, desired Desired, directory project.Directory, routeIDs map[string]string) (resolved[access.CodeBudgetInput], error) {
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
	to, err := mapped(ctx, q, b.ID, projectID, derived, unique, `SELECT id::text,project_id::text,(route_id IS NOT DISTINCT FROM $2::uuid AND api_key_id IS NOT DISTINCT FROM $3::uuid),(route_id IS NOT DISTINCT FROM $2::uuid AND api_key_id IS NOT DISTINCT FROM $3::uuid AND (daily_tokens IS NOT NULL)=$4 AND (monthly_tokens IS NOT NULL)=$5) FROM olp.code_token_budgets WHERE id=$6 OR id=$7 OR (project_id=$1 AND route_id IS NOT DISTINCT FROM $2::uuid AND api_key_id IS NOT DISTINCT FROM $3::uuid AND (daily_tokens IS NOT NULL)=$4 AND (monthly_tokens IS NOT NULL)=$5)`, projectID, routeID, keyID, b.DailyTokens != nil, b.MonthlyTokens != nil, b.ID, derived)
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
	action, err := compared(ctx, q, b, `SELECT jsonb_build_object('project_id',project_id,'route_id',route_id,'api_key_id',api_key_id,'daily_tokens',daily_tokens,'monthly_tokens',monthly_tokens,'enabled',enabled) FROM olp.code_token_budgets WHERE id=$1`)
	return change{
		item: promotion.Item{Kind: contract.ConfigurationPlanItemKindCodeBudget, Key: b.ref, Action: action},
		write: func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
			_, err := access.SaveCodeBudget(ctx, tx, p, b.to.id, access.NewID(), b.input, b.to.existing)
			return err
		},
	}, err
}
