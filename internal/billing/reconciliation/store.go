package reconciliation

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage"
)

type providerImport struct {
	ProviderID    uuid.UUID
	LastSuccessAt *time.Time
	LastErrorCode *string
	Active        bool
}

type cellKey struct {
	ProviderID uuid.UUID
	Day        time.Time
}

func (k cellKey) key() cellKey {
	return k
}

type coveredDay struct {
	cellKey
	LastSuccessAt *time.Time
}

type invoicedDay struct {
	cellKey
	Amount     decimal.Decimal
	Currencies []string
}

type daySpend struct {
	cellKey
	Attributed             decimal.Decimal
	PricedAttempts         int64
	UnpricedAttempts       int64
	IncompleteAttempts     int64
	ExcludedCallerAttempts int64
}

func visibleImports(ctx context.Context, q access.Queryer, p access.Principal, provider uuid.NullUUID) ([]providerImport, error) {
	rows, err := q.Query(ctx, `SELECT pr.id,
		CASE WHEN bi.provider_id IS NOT NULL THEN bi.last_success_at ELSE bc.last_success_at END,
		bi.last_error_code, bi.provider_id IS NOT NULL
		FROM olp.providers pr
		LEFT JOIN olp.provider_billing_imports bi ON bi.provider_id=pr.id
		LEFT JOIN olp.provider_billing_contexts bc ON bc.provider_id=pr.id
		WHERE ($1::uuid IS NULL OR pr.id=$1)
		  AND ($2 OR pr.project_id::text=ANY($3::text[]))
		  AND (bi.provider_id IS NOT NULL OR bc.provider_id IS NOT NULL
		       OR EXISTS(SELECT 1 FROM olp.provider_billing_days bd WHERE bd.provider_id=pr.id))`,
		provider, p.AllProjects, p.ProjectIDs())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[providerImport])
}

func importedProviders(imports []providerImport) []uuid.UUID {
	providers := make([]uuid.UUID, len(imports))
	for i, imp := range imports {
		providers[i] = imp.ProviderID
	}
	return providers
}

func coveredDays(ctx context.Context, q access.Queryer, providers []uuid.UUID, window Range) ([]coveredDay, error) {
	rows, err := q.Query(ctx, `SELECT provider_id, day, last_success_at
		FROM olp.provider_billing_days
		WHERE provider_id=ANY($1::uuid[]) AND day>=$2::date AND day<$3::date`,
		providers, window.Start, window.End)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[coveredDay])
}

func invoicedDays(ctx context.Context, q access.Queryer, providers []uuid.UUID, window Range) ([]invoicedDay, error) {
	rows, err := q.Query(ctx, `SELECT provider_id, day, sum(amount),
		array_agg(DISTINCT btrim(currency) ORDER BY btrim(currency))
		FROM olp.provider_invoiced_costs
		WHERE provider_id=ANY($1::uuid[]) AND day>=$2::date AND day<$3::date
		GROUP BY provider_id, day`,
		providers, window.Start, window.End)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[invoicedDay])
}

func attributedSpend(ctx context.Context, q access.Queryer, providers []uuid.UUID, window Range) ([]daySpend, error) {
	ids := make([]string, len(providers))
	for i, provider := range providers {
		ids[i] = provider.String()
	}
	rows, err := usage.ReconciliationSpend(ctx, q, window.Start, window.End, ids)
	if err != nil {
		return nil, err
	}
	spend := make([]daySpend, len(rows))
	for i, row := range rows {
		parsed, err := parseSpend(row)
		if err != nil {
			return nil, err
		}
		spend[i] = parsed
	}
	return spend, nil
}

func parseSpend(row usage.ProviderDaySpend) (daySpend, error) {
	provider, err := uuid.Parse(row.ProviderID)
	if err != nil {
		return daySpend{}, err
	}
	day, err := time.Parse(time.DateOnly, row.Day)
	if err != nil {
		return daySpend{}, err
	}
	attributed, err := decimal.NewFromString(row.AttributedCost)
	if err != nil {
		return daySpend{}, err
	}
	return daySpend{
		cellKey:                cellKey{ProviderID: provider, Day: day},
		Attributed:             attributed,
		PricedAttempts:         row.PricedAttempts,
		UnpricedAttempts:       row.UnpricedAttempts,
		IncompleteAttempts:     row.IncompleteAttempts,
		ExcludedCallerAttempts: row.ExcludedCallerAttempts,
	}, nil
}
