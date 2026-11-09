package access

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type CodeBudget struct {
	ID            string
	Project       string
	RouteID       *string
	APIKeyID      *string
	DailyTokens   *int64
	MonthlyTokens *int64
	Enabled       bool
}

type CodeBudgetMatch struct {
	ProjectID string
	RouteID   *string
	APIKeyID  *string
	Daily     bool
	Monthly   bool
}

func ReadCodeBudgets(ctx context.Context, q Queryer) ([]CodeBudget, error) {
	rows, err := q.Query(ctx, `SELECT x.id::text,p.name,x.route_id::text,x.api_key_id::text,x.daily_tokens,x.monthly_tokens,x.enabled FROM olp.code_token_budgets x JOIN olp.projects p ON p.id=x.project_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[CodeBudget])
}

func CodeBudgetCandidates(ctx context.Context, q Queryer, ref, derived string, match CodeBudgetMatch) ([]CodeCandidate, error) {
	rows, err := q.Query(ctx, `SELECT id::text,project_id::text,(route_id IS NOT DISTINCT FROM $2::uuid AND api_key_id IS NOT DISTINCT FROM $3::uuid),(route_id IS NOT DISTINCT FROM $2::uuid AND api_key_id IS NOT DISTINCT FROM $3::uuid AND (daily_tokens IS NOT NULL)=$4 AND (monthly_tokens IS NOT NULL)=$5) FROM olp.code_token_budgets WHERE id=$6 OR id=$7 OR (project_id=$1 AND route_id IS NOT DISTINCT FROM $2::uuid AND api_key_id IS NOT DISTINCT FROM $3::uuid AND (daily_tokens IS NOT NULL)=$4 AND (monthly_tokens IS NOT NULL)=$5)`, match.ProjectID, match.RouteID, match.APIKeyID, match.Daily, match.Monthly, ref, derived)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[CodeCandidate])
}

func ReadCodeBudgetInput(ctx context.Context, q Queryer, id string) (CodeBudgetInput, error) {
	var input CodeBudgetInput
	err := q.QueryRow(ctx, `SELECT jsonb_build_object('project_id',project_id,'route_id',route_id,'api_key_id',api_key_id,'daily_tokens',daily_tokens,'monthly_tokens',monthly_tokens,'enabled',enabled) FROM olp.code_token_budgets WHERE id=$1`, id).Scan(&input)
	return input, err
}
