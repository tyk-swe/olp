package budget

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/code"
)

type Budget struct {
	ID            string  `json:"id"`
	ProjectID     string  `json:"project_id"`
	RouteID       *string `json:"route_id"`
	APIKeyID      *string `json:"api_key_id"`
	DailyTokens   *int64  `json:"daily_tokens"`
	MonthlyTokens *int64  `json:"monthly_tokens"`
	Enabled       bool    `json:"enabled"`
	ETag          string  `json:"etag"`
}

type TokenWindow struct {
	Period   string    `json:"period"`
	StartsAt time.Time `json:"starts_at"`
	Reserved int64     `json:"reserved"`
	Measured int64     `json:"measured"`
}

type TokenWindows struct {
	ID        string        `json:"id"`
	ProjectID string        `json:"project_id"`
	Windows   []TokenWindow `json:"windows"`
}

const selectRows = `SELECT x.id::text, x.project_id::text, x.route_id::text, x.api_key_id::text, x.daily_tokens, x.monthly_tokens, x.enabled, x.etag::text FROM olp.code_token_budgets x`

func scanBudget(row pgx.Row) (Budget, error) {
	var b Budget
	err := row.Scan(&b.ID, &b.ProjectID, &b.RouteID, &b.APIKeyID, &b.DailyTokens, &b.MonthlyTokens, &b.Enabled, &b.ETag)
	return b, err
}

func Load(ctx context.Context, q access.Queryer, id string) (Budget, error) {
	return scanBudget(q.QueryRow(ctx, selectRows+` WHERE x.id=$1`, id))
}

func Save(ctx context.Context, tx pgx.Tx, p access.Principal, id, etag string, d Draft, existing bool) (Budget, error) {
	project := d.projectID
	if err := p.Project(&project, access.Change); err != nil {
		return Budget{}, err
	}
	if d.routeID != nil {
		var matches bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_routes WHERE id=$1 AND project_id=$2)`, *d.routeID, d.projectID).Scan(&matches); err != nil {
			return Budget{}, err
		}
		if !matches {
			return Budget{}, access.NotFound
		}
	}
	if d.apiKeyID != nil {
		var matches bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.api_keys WHERE id=$1 AND project_id=$2)`, *d.apiKeyID, d.projectID).Scan(&matches); err != nil {
			return Budget{}, err
		}
		if !matches {
			return Budget{}, access.NotFound
		}
	}
	if existing {
		var matches bool
		if err := tx.QueryRow(ctx, `SELECT route_id IS NOT DISTINCT FROM $2::uuid AND api_key_id IS NOT DISTINCT FROM $3::uuid FROM olp.code_token_budgets WHERE id=$1`, id, d.routeID, d.apiKeyID).Scan(&matches); err != nil {
			return Budget{}, err
		}
		if !matches {
			return Budget{}, access.Invalid("scope", "Budget scope is immutable.")
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.code_token_budgets(id,project_id,route_id,api_key_id,daily_tokens,monthly_tokens,enabled,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT(id) DO UPDATE SET daily_tokens=excluded.daily_tokens,monthly_tokens=excluded.monthly_tokens,enabled=excluded.enabled,etag=excluded.etag`,
		id, d.projectID, d.routeID, d.apiKeyID, d.dailyTokens, d.monthlyTokens, d.enabled, etag, p.UserID()); err != nil {
		return Budget{}, err
	}
	return Load(ctx, tx, id)
}

func scanTokenWindows(row pgx.Row) (TokenWindows, error) {
	var item TokenWindows
	var windowsJSON []byte
	if err := row.Scan(&item.ID, &item.ProjectID, &windowsJSON); err != nil {
		return TokenWindows{}, err
	}
	item.Windows = []TokenWindow{}
	if len(windowsJSON) > 0 {
		if err := json.Unmarshal(windowsJSON, &item.Windows); err != nil {
			return TokenWindows{}, err
		}
	}
	return item, nil
}

type Stored struct {
	ID            string
	Project       string
	RouteID       *string
	APIKeyID      *string
	DailyTokens   *int64
	MonthlyTokens *int64
	Enabled       bool
}

type Match struct {
	ProjectID string
	RouteID   *string
	APIKeyID  *string
	Daily     bool
	Monthly   bool
}

func ReadStored(ctx context.Context, q access.Queryer) ([]Stored, error) {
	rows, err := q.Query(ctx, `SELECT x.id::text,p.name,x.route_id::text,x.api_key_id::text,x.daily_tokens,x.monthly_tokens,x.enabled FROM olp.code_token_budgets x JOIN olp.projects p ON p.id=x.project_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Stored])
}

func Candidates(ctx context.Context, q access.Queryer, ref, derived string, match Match) ([]code.CandidateMatch, error) {
	rows, err := q.Query(ctx, `SELECT id::text,project_id::text,(route_id IS NOT DISTINCT FROM $2::uuid AND api_key_id IS NOT DISTINCT FROM $3::uuid),(route_id IS NOT DISTINCT FROM $2::uuid AND api_key_id IS NOT DISTINCT FROM $3::uuid AND (daily_tokens IS NOT NULL)=$4 AND (monthly_tokens IS NOT NULL)=$5) FROM olp.code_token_budgets WHERE id=$6 OR id=$7 OR (project_id=$1 AND route_id IS NOT DISTINCT FROM $2::uuid AND api_key_id IS NOT DISTINCT FROM $3::uuid AND (daily_tokens IS NOT NULL)=$4 AND (monthly_tokens IS NOT NULL)=$5)`, match.ProjectID, match.RouteID, match.APIKeyID, match.Daily, match.Monthly, ref, derived)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[code.CandidateMatch])
}

func ReadInput(ctx context.Context, q access.Queryer, id string) (Input, error) {
	var input Input
	err := q.QueryRow(ctx, `SELECT jsonb_build_object('project_id',project_id,'route_id',route_id,'api_key_id',api_key_id,'daily_tokens',daily_tokens,'monthly_tokens',monthly_tokens,'enabled',enabled) FROM olp.code_token_budgets WHERE id=$1`, id).Scan(&input)
	return input, err
}
