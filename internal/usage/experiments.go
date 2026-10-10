package usage

import (
	"cmp"
	"context"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

// maxReportRows bounds the groups a selector or experiment report returns.
const maxReportRows = 500

// SelectorSavings compares what one route selector's attempts cost with what
// their own usage would have cost on the most expensive target the selector
// avoided, priced when each attempt was accounted. Only attempts with both
// prices are compared, so the report never invents usage.
type SelectorSavings struct {
	Route            string `json:"route"`
	Selector         string `json:"selector"`
	Attempts         int64  `json:"attempts"`
	ComparedAttempts int64  `json:"compared_attempts"`
	Cost             string `json:"estimated_cost"`
	BaselineCost     string `json:"baseline_cost"`
	Savings          string `json:"savings"`
}

// SelectorSavingsReport is the savings of every selector that served traffic
// in a range.
type SelectorSavingsReport struct {
	Start    time.Time         `json:"start"`
	End      time.Time         `json:"end"`
	Currency *string           `json:"currency"`
	Items    []SelectorSavings `json:"items"`
}

// ExperimentSide summarizes one path of a shadow experiment: the primary
// requests a shadow target mirrored, or the shadow requests themselves.
type ExperimentSide struct {
	Successes        int64    `json:"successes"`
	MeanLatencyMS    *float64 `json:"mean_latency_ms"`
	MeanFirstByteMS  *float64 `json:"mean_first_byte_ms"`
	InputTokens      string   `json:"input_tokens"`
	OutputTokens     string   `json:"output_tokens"`
	EstimatedCost    *string  `json:"estimated_cost"`
	UnpricedRequests int64    `json:"unpriced_requests"`
}

// Experiment compares a route's primary path with one shadow target over the
// request pairs the shadow mirrored.
type Experiment struct {
	Route         string         `json:"route"`
	ProviderID    string         `json:"provider_id"`
	UpstreamModel string         `json:"upstream_model"`
	Pairs         int64          `json:"pairs"`
	Primary       ExperimentSide `json:"primary"`
	Shadow        ExperimentSide `json:"shadow"`
}

// ExperimentReport is every shadow experiment that mirrored traffic in a
// range, from request metadata alone.
type ExperimentReport struct {
	Start    time.Time    `json:"start"`
	End      time.Time    `json:"end"`
	Currency *string      `json:"currency"`
	Items    []Experiment `json:"items"`
}

// routeScope restricts a report to the routes of the projects a principal may
// see. Selector and shadow traffic belongs to routes, and shadow requests
// carry no key, so the route's project scopes them.
func (f Filters) routeScope(q *filterQuery, column string) {
	if !f.AllProjects {
		q.push(" AND " + column + " IN (SELECT slug FROM olp.routes WHERE project_id = ANY(" + q.bind(f.AllowedProjects) + "::uuid[]))")
	}
	if f.Route != nil {
		q.pushBind(" AND "+column+" = ", *f.Route)
	}
}

// ReadSelectorSavings reports each selector's savings in the range.
func ReadSelectorSavings(ctx context.Context, q access.Queryer, f Filters) (SelectorSavingsReport, error) {
	report := SelectorSavingsReport{Start: f.Start, End: f.End, Items: []SelectorSavings{}}
	var query filterQuery
	query.push(`SELECT route_slug, selector, count(*),
        count(baseline_cost),
        COALESCE(sum(estimated_cost) FILTER (WHERE baseline_cost IS NOT NULL), 0)::text,
        COALESCE(sum(baseline_cost), 0)::text,
        COALESCE(sum(baseline_cost - estimated_cost), 0)::text,
        max(btrim(currency))
    FROM olp.attempt_usage_facts
    WHERE selector IS NOT NULL AND observed_at >= `)
	query.push(query.bind(f.Start) + " AND observed_at < " + query.bind(f.End))
	f.routeScope(&query, "route_slug")
	query.push(" GROUP BY route_slug, selector ORDER BY route_slug, selector LIMIT " + query.bind(maxReportRows))
	rows, err := q.Query(ctx, query.sql(), query.args...)
	if err != nil {
		return report, err
	}
	defer rows.Close()
	for rows.Next() {
		var item SelectorSavings
		var currency *string
		if err = rows.Scan(&item.Route, &item.Selector, &item.Attempts, &item.ComparedAttempts, &item.Cost, &item.BaselineCost, &item.Savings, &currency); err != nil {
			return report, err
		}
		report.Items = append(report.Items, item)
		report.Currency = cmp.Or(report.Currency, currency)
	}
	if err = rows.Err(); err != nil {
		return report, err
	}
	report.Currency, err = labelCurrency(ctx, q, report.Currency)
	return report, err
}

// labelCurrency is the currency the report's own rows carry, falling back to
// the installation's so a report without priced rows still labels its zero.
func labelCurrency(ctx context.Context, q access.Queryer, currency *string) (*string, error) {
	if currency != nil {
		return currency, nil
	}
	err := q.QueryRow(ctx, "SELECT (SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton)").Scan(&currency)
	return currency, err
}

// experimentLag bounds how long after its parent began a shadow request can
// begin: the parent finishes within its route's deadline, at most an hour,
// before the shadow is launched. It lets both partitions be pruned.
const experimentLag = 2 * time.Hour

// ReadExperiments compares each shadow target with the primary path of the
// requests it mirrored in the range.
func ReadExperiments(ctx context.Context, q access.Queryer, f Filters) (ExperimentReport, error) {
	report := ExperimentReport{Start: f.Start, End: f.End, Items: []Experiment{}}
	var query filterQuery
	start, end := query.bind(f.Start), query.bind(f.End)
	query.push(`WITH pairs AS (
        SELECT shadow.route_slug, shadow.id AS shadow_id, shadow.started_at AS shadow_started,
               shadow.status_code AS shadow_status, shadow.total_latency_ms AS shadow_latency,
               shadow.first_byte_ms AS shadow_first_byte,
               parent.id AS primary_id, parent.started_at AS primary_started,
               parent.status_code AS primary_status, parent.total_latency_ms AS primary_latency,
               parent.first_byte_ms AS primary_first_byte
          FROM olp.requests shadow
          JOIN olp.requests parent ON parent.id = shadow.parent_request_id
           AND parent.started_at <= shadow.started_at
           AND parent.started_at > shadow.started_at - ` + query.bind(experimentLag.String()) + `::interval
         WHERE shadow.origin = 'shadow' AND shadow.started_at >= ` + start + ` AND shadow.started_at < ` + end)
	f.routeScope(&query, "shadow.route_slug")
	query.push(`),
    charged AS (
        SELECT request_id, min(provider_id::text) FILTER (WHERE attempt_ordinal = 1) AS provider_id,
               min(upstream_model) FILTER (WHERE attempt_ordinal = 1) AS upstream_model,
               COALESCE(sum(input_tokens), 0) AS input_tokens, COALESCE(sum(output_tokens), 0) AS output_tokens,
               sum(estimated_cost) AS cost, bool_or(unpriced) AS unpriced, max(btrim(currency)) AS currency
          FROM olp.attempt_usage_facts
         WHERE request_started_at > ` + start + `::timestamptz - ` + query.bind(experimentLag.String()) + `::interval
           AND request_started_at < ` + end + `
           AND request_id IN (SELECT shadow_id FROM pairs UNION ALL SELECT primary_id FROM pairs)
         GROUP BY request_id)
    SELECT pairs.route_slug, shadow.provider_id, shadow.upstream_model, count(*),
           count(*) FILTER (WHERE primary_status BETWEEN 200 AND 299),
           avg(primary_latency)::float8, avg(primary_first_byte)::float8,
           COALESCE(sum(main.input_tokens), 0)::text, COALESCE(sum(main.output_tokens), 0)::text,
           sum(main.cost)::text, count(*) FILTER (WHERE main.unpriced),
           count(*) FILTER (WHERE shadow_status BETWEEN 200 AND 299),
           avg(shadow_latency)::float8, avg(shadow_first_byte)::float8,
           COALESCE(sum(shadow.input_tokens), 0)::text, COALESCE(sum(shadow.output_tokens), 0)::text,
           sum(shadow.cost)::text, count(*) FILTER (WHERE shadow.unpriced),
           COALESCE(max(shadow.currency), max(main.currency))
      FROM pairs
      JOIN charged shadow ON shadow.request_id = pairs.shadow_id
      LEFT JOIN charged main ON main.request_id = pairs.primary_id
     WHERE shadow.provider_id IS NOT NULL
     GROUP BY pairs.route_slug, shadow.provider_id, shadow.upstream_model
     ORDER BY pairs.route_slug, shadow.provider_id, shadow.upstream_model
     LIMIT ` + query.bind(maxReportRows))
	rows, err := q.Query(ctx, query.sql(), query.args...)
	if err != nil {
		return report, err
	}
	defer rows.Close()
	for rows.Next() {
		var item Experiment
		var currency *string
		p, s := &item.Primary, &item.Shadow
		if err = rows.Scan(&item.Route, &item.ProviderID, &item.UpstreamModel, &item.Pairs,
			&p.Successes, &p.MeanLatencyMS, &p.MeanFirstByteMS, &p.InputTokens, &p.OutputTokens, &p.EstimatedCost, &p.UnpricedRequests,
			&s.Successes, &s.MeanLatencyMS, &s.MeanFirstByteMS, &s.InputTokens, &s.OutputTokens, &s.EstimatedCost, &s.UnpricedRequests,
			&currency,
		); err != nil {
			return report, err
		}
		report.Items = append(report.Items, item)
		report.Currency = cmp.Or(report.Currency, currency)
	}
	if err = rows.Err(); err != nil {
		return report, err
	}
	report.Currency, err = labelCurrency(ctx, q, report.Currency)
	return report, err
}

func (s *Server) selectorSavings(r *http.Request, p access.Principal) (access.Reply, error) {
	filters, err := usageFilters(r, p)
	if err != nil {
		return access.Reply{}, err
	}
	report, err := ReadSelectorSavings(r.Context(), s.Access.Pool, filters)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(report), nil
}

func (s *Server) shadowExperiments(r *http.Request, p access.Principal) (access.Reply, error) {
	filters, err := usageFilters(r, p)
	if err != nil {
		return access.Reply{}, err
	}
	report, err := ReadExperiments(r.Context(), s.Access.Pool, filters)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(report), nil
}
