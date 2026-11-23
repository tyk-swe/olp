package reports

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/usage/param"
)

const comparisonDay = 24 * time.Hour

type comparisonState string

const (
	comparisonAvailable            comparisonState = "available"
	comparisonIncomplete           comparisonState = "incomplete"
	comparisonIncompatibleCurrency comparisonState = "incompatible_currency"
	comparisonNoPricedUsage        comparisonState = "no_priced_usage"
)

type usageComparison struct {
	Previous comparisonPeriod   `json:"previous"`
	Current  comparisonPeriod   `json:"current"`
	Status   comparisonState    `json:"status"`
	Currency *string            `json:"currency"`
	Items    []routeSpendChange `json:"items"`
}

type comparisonPeriod struct {
	Start        time.Time      `json:"start"`
	End          time.Time      `json:"end"`
	Completeness Completeness   `json:"completeness"`
	Subtotals    []costSubtotal `json:"subtotals"`
}

type costSubtotal struct {
	Currency      *string `json:"currency"`
	EstimatedCost string  `json:"estimated_cost"`
}

type routeSpendChange struct {
	Route         string  `json:"route"`
	PreviousCost  string  `json:"previous_cost"`
	CurrentCost   string  `json:"current_cost"`
	Change        string  `json:"change"`
	PercentChange *string `json:"percent_change"`
}

func comparisonBaseline(f Filters, now time.Time) (Filters, error) {
	duration := f.End.Sub(f.Start)
	if duration < comparisonDay || duration > 31*comparisonDay ||
		!f.Start.Equal(f.Start.Truncate(comparisonDay)) || !f.End.Equal(f.End.Truncate(comparisonDay)) ||
		f.End.After(now.Truncate(comparisonDay)) {
		return Filters{}, fail(FailureInvalidRange, "Choose 1 to 31 closed UTC days with midnight boundaries.")
	}
	return f.withRange(f.Start.Add(-duration), f.Start), nil
}

func (s *API) usageComparison(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	query := r.URL.Query()
	if len(query["project_id"]) != 1 || param.Text(query, "project_id") == nil {
		return httpkit.Reply{}, fail(FailureInvalidFilter, "Select one project to compare its usage.")
	}
	filters, err := usageFilters(r, p)
	if err != nil {
		return httpkit.Reply{}, err
	}
	now := time.Now()
	previous, err := comparisonBaseline(filters, now)
	if err != nil {
		return httpkit.Reply{}, err
	}
	ctx := r.Context()
	tx, err := s.Access.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	report, err := readComparison(ctx, tx, previous, filters, now)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.OK(report), nil
}

func readComparison(ctx context.Context, q database.Queryer, previous, current Filters, now time.Time) (usageComparison, error) {
	prior, err := readComparisonPeriod(ctx, q, previous, now)
	if err != nil {
		return usageComparison{}, err
	}
	selected, err := readComparisonPeriod(ctx, q, current, now)
	if err != nil {
		return usageComparison{}, err
	}
	status, currency := comparisonStatus(prior, selected)
	items := []routeSpendChange{}
	if status == comparisonAvailable {
		items, err = readRouteSpendChanges(ctx, q, previous, current)
		if err != nil {
			return usageComparison{}, err
		}
	}
	return usageComparison{Previous: prior, Current: selected, Status: status, Currency: currency, Items: items}, nil
}

func comparisonStatus(previous, current comparisonPeriod) (comparisonState, *string) {
	if !previous.Completeness.Complete || !current.Completeness.Complete {
		return comparisonIncomplete, nil
	}
	var currency *string
	for _, subtotal := range slices.Concat(previous.Subtotals, current.Subtotals) {
		if subtotal.Currency == nil || (currency != nil && *subtotal.Currency != *currency) {
			return comparisonIncompatibleCurrency, nil
		}
		currency = subtotal.Currency
	}
	if currency == nil {
		return comparisonNoPricedUsage, nil
	}
	return comparisonAvailable, currency
}

func readComparisonPeriod(ctx context.Context, q database.Queryer, f Filters, now time.Time) (comparisonPeriod, error) {
	var query filterQuery
	f.completenessRows(&query)
	query.push(`, currency_totals AS (
		SELECT CASE WHEN estimated_cost IS NOT NULL THEN btrim(currency) END AS currency,
			` + completenessCountsWithAliases + `, SUM(estimated_cost) AS cost
		FROM completeness_rows GROUP BY 1
	)
	SELECT ` + completenessCounts + `,
		COALESCE(jsonb_agg(jsonb_build_object('currency', currency, 'estimated_cost', trim_scale(cost)::text)
			ORDER BY currency) FILTER (WHERE cost IS NOT NULL), '[]'::jsonb)
	FROM currency_totals`)
	var report Completeness
	var subtotals []costSubtotal
	if err := q.QueryRow(ctx, query.sql(), query.args...).Scan(&report.RequestCount, &report.UnpricedCount,
		&report.IncompleteCount, &subtotals); err != nil {
		return comparisonPeriod{}, fmt.Errorf("read usage comparison subtotals: %w", err)
	}
	if len(subtotals) == 1 {
		report.EstimatedCost, report.Currency = &subtotals[0].EstimatedCost, subtotals[0].Currency
	}
	evidence, err := withCompletenessEvidence(ctx, q, f, now, report)
	return comparisonPeriod{Start: f.Start, End: f.End, Completeness: evidence, Subtotals: subtotals}, err
}

const completenessCountsWithAliases = "SUM(request_count) AS request_count, SUM(unpriced_count) AS unpriced_count, SUM(incomplete_count) AS incomplete_count"

func readRouteSpendChanges(ctx context.Context, q database.Queryer, previous, current Filters) ([]routeSpendChange, error) {
	combined := current.withRange(previous.Start, current.End)
	var query filterQuery
	combined.completenessRows(&query)
	boundary := query.bind(current.Start)
	query.push(`, route_costs AS (
		SELECT route_slug,
			COALESCE(SUM(estimated_cost) FILTER (WHERE observed_at < ` + boundary + `), 0) AS previous_cost,
			COALESCE(SUM(estimated_cost) FILTER (WHERE observed_at >= ` + boundary + `), 0) AS current_cost
		FROM completeness_rows WHERE estimated_cost IS NOT NULL GROUP BY route_slug
	)
	SELECT route_slug, trim_scale(previous_cost)::text, trim_scale(current_cost)::text,
		trim_scale(current_cost - previous_cost)::text,
		CASE WHEN previous_cost > 0 THEN trim_scale(round(100 * (current_cost - previous_cost) / previous_cost, 6))::text END
	FROM route_costs ORDER BY abs(current_cost - previous_cost) DESC, route_slug COLLATE "C" LIMIT 25`)
	rows, err := q.Query(ctx, query.sql(), query.args...)
	if err != nil {
		return nil, fmt.Errorf("read route spend changes: %w", err)
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[routeSpendChange])
	if err != nil {
		return nil, fmt.Errorf("read route spend changes: %w", err)
	}
	return items, nil
}
