package codeadmin

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/platform/database"
)

type Budget struct {
	ID            string
	Project       string
	RouteID       *string
	APIKeyID      *string
	DailyTokens   *int64
	MonthlyTokens *int64
	Enabled       bool
}

type BudgetMatch struct {
	ProjectID string
	RouteID   *string
	APIKeyID  *string
	Daily     bool
	Monthly   bool
}

func ReadBudgets(ctx context.Context, q database.Queryer) ([]Budget, error) {
	rows, err := q.Query(ctx, `SELECT x.id::text,p.name,x.route_id::text,x.api_key_id::text,x.daily_tokens,x.monthly_tokens,x.enabled FROM olp.code_token_budgets x JOIN olp.projects p ON p.id=x.project_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Budget])
}

func BudgetCandidates(ctx context.Context, q database.Queryer, ref, derived string, match BudgetMatch) ([]Candidate, error) {
	rows, err := q.Query(ctx, `SELECT id::text,project_id::text,(route_id IS NOT DISTINCT FROM $2::uuid AND api_key_id IS NOT DISTINCT FROM $3::uuid),(route_id IS NOT DISTINCT FROM $2::uuid AND api_key_id IS NOT DISTINCT FROM $3::uuid AND (daily_tokens IS NOT NULL)=$4 AND (monthly_tokens IS NOT NULL)=$5) FROM olp.code_token_budgets WHERE id=$6 OR id=$7 OR (project_id=$1 AND route_id IS NOT DISTINCT FROM $2::uuid AND api_key_id IS NOT DISTINCT FROM $3::uuid AND (daily_tokens IS NOT NULL)=$4 AND (monthly_tokens IS NOT NULL)=$5)`, match.ProjectID, match.RouteID, match.APIKeyID, match.Daily, match.Monthly, ref, derived)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Candidate])
}

func ReadBudgetInput(ctx context.Context, q database.Queryer, id string) (BudgetInput, error) {
	var input BudgetInput
	err := q.QueryRow(ctx, `SELECT jsonb_build_object('project_id',project_id,'route_id',route_id,'api_key_id',api_key_id,'daily_tokens',daily_tokens,'monthly_tokens',monthly_tokens,'enabled',enabled) FROM olp.code_token_budgets WHERE id=$1`, id).Scan(&input)
	return input, err
}

type BudgetInput struct {
	ProjectID     string  `json:"project_id"`
	RouteID       *string `json:"route_id"`
	APIKeyID      *string `json:"api_key_id"`
	DailyTokens   *int64  `json:"daily_tokens"`
	MonthlyTokens *int64  `json:"monthly_tokens"`
	Enabled       bool    `json:"enabled"`
}

func SaveBudget(ctx context.Context, tx pgx.Tx, p authz.Principal, id, etag string, raw BudgetInput, existing bool) (json.RawMessage, error) {
	in, validationError := raw.Validated()
	if validationError != nil {
		return nil, validationError
	}
	if err := p.Project(&in.ProjectID, authz.Change); err != nil {
		return nil, err
	}
	if in.RouteID != nil {
		if err := scoped(ctx, tx, "code_routes", *in.RouteID, in.ProjectID); err != nil {
			return nil, err
		}
	}
	if in.APIKeyID != nil {
		if err := scoped(ctx, tx, "api_keys", *in.APIKeyID, in.ProjectID); err != nil {
			return nil, err
		}
	}
	if existing {
		var matches bool
		if err := tx.QueryRow(ctx, `SELECT route_id IS NOT DISTINCT FROM $2::uuid AND api_key_id IS NOT DISTINCT FROM $3::uuid FROM olp.code_token_budgets WHERE id=$1`, id, in.RouteID, in.APIKeyID).Scan(&matches); err != nil {
			return nil, err
		}
		if !matches {
			return nil, httpkit.Invalid("scope", "Budget scope is immutable.")
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO olp.code_token_budgets(id,project_id,route_id,api_key_id,daily_tokens,monthly_tokens,enabled,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT(id) DO UPDATE SET daily_tokens=excluded.daily_tokens,monthly_tokens=excluded.monthly_tokens,enabled=excluded.enabled,etag=excluded.etag`, id, in.ProjectID, in.RouteID, in.APIKeyID, in.DailyTokens, in.MonthlyTokens, in.Enabled, etag, p.UserID())
	if err != nil {
		return nil, err
	}
	var body json.RawMessage
	err = tx.QueryRow(ctx, `SELECT to_jsonb(x)-'created_by' FROM olp.code_token_budgets x WHERE id=$1`, id).Scan(&body)
	return body, err
}

func (in BudgetInput) validate() error {
	if in.DailyTokens == nil && in.MonthlyTokens == nil {
		return httpkit.Invalid("tokens", "Set a daily or monthly token cap.")
	}
	for _, n := range []*int64{in.DailyTokens, in.MonthlyTokens} {
		if n != nil && (*n < 1 || *n > 1<<53-1) {
			return httpkit.Invalid("tokens", "Use a positive safe integer.")
		}
	}
	return nil
}

func (in BudgetInput) Validated() (BudgetInput, error) {
	project, projectError := httpkit.ParseUUID(in.ProjectID)
	if projectError != nil {
		return BudgetInput{}, projectError
	}
	route, routeError := httpkit.ParseOptionalUUID(in.RouteID)
	if routeError != nil {
		return BudgetInput{}, routeError
	}
	key, keyError := httpkit.ParseOptionalUUID(in.APIKeyID)
	if keyError != nil {
		return BudgetInput{}, keyError
	}
	canonical := BudgetInput{ProjectID: project, RouteID: route, APIKeyID: key, DailyTokens: in.DailyTokens, MonthlyTokens: in.MonthlyTokens, Enabled: in.Enabled}
	return canonical, canonical.validate()
}

func (in BudgetInput) Validate() error { _, err := in.Validated(); return err }
