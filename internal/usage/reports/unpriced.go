package reports

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/usage/param"
)

func (s *API) unpricedBreakdown(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	filters, err := usageFilters(r, p)
	if err != nil {
		return httpkit.Reply{}, err
	}
	limit, err := param.Limit(r.URL.Query())
	if err != nil {
		return httpkit.Reply{}, err
	}
	report, err := ReadUnpricedBreakdown(r.Context(), s.Access.Pool, filters, limit)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.OK(report), nil
}

func ReadUnpricedBreakdown(ctx context.Context, q database.Queryer, f Filters, limit int) (contract.UnpricedBreakdownResponse, error) {
	if err := f.validate(); err != nil {
		return contract.UnpricedBreakdownResponse{}, err
	}
	b := &filterQuery{}
	b.push(`WITH live AS (
		SELECT provider_id, upstream_model, operation, COALESCE(unpriced_cause, 'not_recorded') AS cause,
		       count(*) AS attempts,
		       max(request_id::text) FILTER (WHERE EXISTS (
		           SELECT 1 FROM olp.requests r
		           WHERE r.id = f.request_id AND r.started_at = f.request_started_at
		             AND r.api_key_id IS NOT DISTINCT FROM f.api_key_id
		       )) AS request_id
		FROM olp.attempt_usage_facts f WHERE unpriced AND charge_status <> 'not_billable'`)
	b.pushBind(" AND observed_at >= ", f.Start)
	b.pushBind(" AND observed_at < ", f.End)
	f.dimensions(b)
	b.push(` GROUP BY provider_id, upstream_model, operation, COALESCE(unpriced_cause, 'not_recorded')
	), combined AS (
		SELECT * FROM live
		UNION ALL
		SELECT provider_id, upstream_model, operation, cause, attempts, NULL::text
		FROM olp.attempt_usage_hourly CROSS JOIN LATERAL (VALUES
		    ('no_applicable_price', no_applicable_price_count),
		    ('missing_price_component', missing_price_component_count),
		    ('incomplete_usage', incomplete_usage_unpriced_count),
		    ('not_recorded', unpriced_attempt_count - no_applicable_price_count
		        - missing_price_component_count - incomplete_usage_unpriced_count)
		) counts(cause, attempts)
		WHERE unpriced_attempt_count > 0 AND attempts > 0`)
	b.pushBind(" AND bucket >= ", ceilHour(f.Start))
	b.pushBind(" AND bucket + interval '1 hour' <= ", f.End)
	f.dimensions(b)
	b.push(`) SELECT provider_id, upstream_model AS model, operation, cause,
		   sum(attempts)::bigint AS attempt_count, max(request_id)::uuid AS sample_request_id
	FROM combined GROUP BY provider_id, upstream_model, operation, cause
	ORDER BY attempt_count DESC, provider_id, upstream_model, operation, cause`)
	b.pushBind(" LIMIT ", limit)
	rows, err := q.Query(ctx, b.sql(), b.args...)
	if err != nil {
		return contract.UnpricedBreakdownResponse{}, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByName[contract.UnpricedBreakdownItem])
	return contract.UnpricedBreakdownResponse{Items: items}, err
}
