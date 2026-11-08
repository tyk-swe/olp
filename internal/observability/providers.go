package observability

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
)

// OperationsSummary is the bounded metadata-only rollup the metrics endpoint
// renders. It reads the same normalized records as the operator API and never
// exposes prompt or response content.
type OperationsSummary struct {
	RequestCount      int64
	SuccessCount      int64
	CancelledAttempts int64
	P95LatencyMs      *float64
	P99LatencyMs      *float64
}

const operationsWindowMinutes = 5

// ReadOperationsSummary rolls up the trailing request window.
func ReadOperationsSummary(ctx context.Context, q access.Queryer) (OperationsSummary, error) {
	minutes := operationsWindowMinutes
	var summary OperationsSummary
	err := q.QueryRow(ctx, `WITH recent_requests AS MATERIALIZED (
			SELECT id, started_at, status_code, error_class, total_latency_ms
			FROM olp.requests
			WHERE started_at >= now() - make_interval(mins => $1)
		)
		SELECT COUNT(*)::bigint,
			COUNT(*) FILTER (WHERE error_class IS NULL
				AND status_code BETWEEN 200 AND 399)::bigint,
			percentile_cont(0.95) WITHIN GROUP (ORDER BY total_latency_ms)
				FILTER (WHERE total_latency_ms IS NOT NULL),
			percentile_cont(0.99) WITHIN GROUP (ORDER BY total_latency_ms)
				FILTER (WHERE total_latency_ms IS NOT NULL),
			(SELECT COUNT(*) FROM olp.attempts a
				JOIN recent_requests r ON r.id = a.request_id
					AND r.started_at = a.request_started_at
				WHERE a.error_class = 'cancelled')::bigint
		FROM recent_requests`, minutes).Scan(
		&summary.RequestCount, &summary.SuccessCount,
		&summary.P95LatencyMs, &summary.P99LatencyMs, &summary.CancelledAttempts)
	if err != nil {
		return summary, fmt.Errorf("read operations summary: %w", err)
	}
	if summary.RequestCount < 0 || summary.SuccessCount < 0 || summary.CancelledAttempts < 0 {
		return summary, errors.New("stored operations summary is invalid")
	}
	return summary, nil
}

// ProviderHealthRecord is one provider's probe- and attempt-window health.
type ProviderHealthRecord struct {
	ProviderID          uuid.UUID
	ProviderName        string
	ProviderKind        string
	ProviderState       string
	Status              Status
	LastProbeAt         *time.Time
	LastProbeStatus     *string
	LastProbeDetail     *string
	LastAttemptAt       *time.Time
	AttemptCount        int64
	SuccessCount        int64
	RateLimitCount      int64
	ServerErrorCount    int64
	TransportErrorCount int64
	AverageLatencyMs    *float64
}

const providerHealthSelect = `SELECT p.id::text, p.name, p.kind, p.state,
		p.last_probe_at, p.last_probe_status, p.last_probe_detail,
		max(a.started_at),
		count(a.id)::bigint,
		count(a.id) FILTER (WHERE a.error_class IS NULL
			AND (a.status_code IS NULL OR a.status_code < 400))::bigint,
		count(a.id) FILTER (WHERE a.status_code = 429
			OR a.error_class = 'rate_limit')::bigint,
		count(a.id) FILTER (WHERE a.status_code >= 500
			OR a.error_class = 'upstream_server')::bigint,
		count(a.id) FILTER (WHERE a.error_class IN
			('connect', 'timeout', 'transport', 'cancelled', 'ambiguous'))::bigint,
		avg(a.latency_ms)::float8
	FROM olp.providers p
	LEFT JOIN olp.attempts a ON a.provider_id = p.id
		AND a.started_at >= now() - make_interval(mins => $1)`

// ProviderHealthPage is one page of the provider-health listing.
type ProviderHealthPage struct {
	Items      []ProviderHealthRecord
	NextCursor *string
}

type ProviderHealthQuery struct {
	WindowMinutes int
	Before        *string
	Limit         int
	AllProjects   bool
	Projects      []string
}

// ReadProviderHealth pages providers in descending id order for the management API.
func ReadProviderHealth(ctx context.Context, q access.Queryer, query ProviderHealthQuery) (ProviderHealthPage, error) {
	rows, err := q.Query(ctx, providerHealthSelect+`
		WHERE ($2::uuid IS NULL OR p.id < $2)
		  AND ($3 OR p.project_id = ANY($4::uuid[]))
		GROUP BY p.id, p.name, p.kind, p.state, p.last_probe_at,
			p.last_probe_status, p.last_probe_detail
		ORDER BY p.id DESC LIMIT $5`, query.WindowMinutes, query.Before, query.AllProjects, query.Projects, query.Limit+1)
	if err != nil {
		return ProviderHealthPage{}, fmt.Errorf("read provider health: %w", err)
	}
	defer rows.Close()
	var records []ProviderHealthRecord
	for rows.Next() {
		record, err := scanProviderHealth(rows)
		if err != nil {
			return ProviderHealthPage{}, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return ProviderHealthPage{}, err
	}
	page := ProviderHealthPage{Items: records}
	if len(records) > query.Limit {
		page.Items = records[:query.Limit]
		next := page.Items[len(page.Items)-1].ProviderID.String()
		page.NextCursor = &next
	}
	return page, nil
}

func scanProviderHealth(rows pgx.Rows) (ProviderHealthRecord, error) {
	var record ProviderHealthRecord
	if err := rows.Scan(&record.ProviderID, &record.ProviderName, &record.ProviderKind,
		&record.ProviderState, &record.LastProbeAt, &record.LastProbeStatus, &record.LastProbeDetail,
		&record.LastAttemptAt, &record.AttemptCount, &record.SuccessCount,
		&record.RateLimitCount, &record.ServerErrorCount, &record.TransportErrorCount,
		&record.AverageLatencyMs); err != nil {
		return record, err
	}
	if record.AttemptCount < 0 || record.SuccessCount < 0 || record.RateLimitCount < 0 ||
		record.ServerErrorCount < 0 || record.TransportErrorCount < 0 {
		return record, errors.New("stored provider health is invalid")
	}
	switch record.ProviderState {
	case "draft", "active", "disabled":
	default:
		return record, errors.New("stored provider state is invalid")
	}
	record.Status = providerHealthStatus(record.ProviderState, record.LastProbeAt,
		record.LastProbeStatus, record.LastAttemptAt, record.AttemptCount, record.SuccessCount)
	return record, nil
}

// ReadProviderHealthMetrics rolls up per-provider attempt health across the
// trailing fifteen minutes. The bool reports completeness: false means the
// snapshot was truncated at the page bound.
func ReadProviderHealthMetrics(ctx context.Context, q access.Queryer) ([]ProviderHealthRecord, bool, error) {
	var records []ProviderHealthRecord
	query := ProviderHealthQuery{WindowMinutes: 15, Limit: 200, AllProjects: true}
	for {
		page, err := ReadProviderHealth(ctx, q, query)
		if err != nil {
			return nil, false, err
		}
		records = append(records, page.Items...)
		if page.NextCursor == nil {
			return records, true, nil
		}
		query.Before = page.NextCursor
		if len(records) >= 10000 {
			return records, false, nil
		}
	}
}

// providerHealthStatus classifies a provider from its durable state, its last
// probe, and the attempt window, mirroring the operator-facing rules.
func providerHealthStatus(state string, probeAt *time.Time, probeStatus *string, lastAttempt *time.Time, attempts, successes int64) Status {
	if state == "disabled" {
		return StatusDisabled
	}
	// A failed probe newer than the newest attempt overrides attempt history.
	probeIsLatest := probeStatus != nil && *probeStatus == "failed" &&
		(probeAt != nil && (lastAttempt == nil || !probeAt.Before(*lastAttempt)))
	if probeIsLatest {
		return StatusUnavailable
	}
	if attempts == 0 {
		switch {
		case probeStatus != nil && *probeStatus == "succeeded":
			return StatusHealthy
		case probeStatus != nil && *probeStatus == "failed":
			return StatusUnavailable
		}
		return StatusUnknown
	}
	failures := attempts - successes
	switch {
	case failures*2 >= attempts:
		return StatusUnavailable
	case failures*10 >= attempts:
		return StatusDegraded
	}
	return StatusHealthy
}
