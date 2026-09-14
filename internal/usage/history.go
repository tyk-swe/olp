package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

// RequestFilters narrow the request explorer. Every field is optional; the
// provider and model filters match a request through its attempts, so a request
// that failed over is found by either provider it touched.
type RequestFilters struct {
	Route         *string
	ProviderID    *string
	Model         *string
	APIKey        *string
	Operation     *string
	StatusCode    *int
	ErrorClass    *string
	StartedAfter  *time.Time
	StartedBefore *time.Time
}

// Cursor is a position in a list ordered by timestamp and identifier.
type Cursor struct {
	At time.Time
	ID string
}

// RequestSummary is one row of the request explorer: the request's own
// metadata plus the usage its attempts accounted for. Token totals and money
// are absent (null) when no fact exists yet, which is different from zero.
type RequestSummary struct {
	ID                  string     `json:"id"`
	RuntimeGenerationID string     `json:"runtime_generation_id"`
	APIKeyID            string     `json:"api_key_id"`
	Route               string     `json:"route"`
	Operation           string     `json:"operation"`
	Surface             string     `json:"surface"`
	StartedAt           time.Time  `json:"started_at"`
	CompletedAt         *time.Time `json:"completed_at"`
	StatusCode          *int32     `json:"status_code"`
	ErrorClass          *string    `json:"error_class"`
	TotalLatencyMS      *int64     `json:"total_latency_ms"`
	FirstByteMS         *int64     `json:"first_byte_ms"`
	AttemptCount        int32      `json:"attempt_count"`
	InputTokens         *int64     `json:"input_tokens"`
	OutputTokens        *int64     `json:"output_tokens"`
	CachedInputTokens   *int64     `json:"cached_input_tokens"`
	EstimatedCost       *string    `json:"estimated_cost"`
	Currency            *string    `json:"currency"`
	Unpriced            *bool      `json:"unpriced"`
	UsageComplete       *bool      `json:"usage_complete"`
}

// AttemptDetail is one provider attempt of a request, with the routing
// provenance that explains why this provider served it and the charge evidence
// that explains what it cost.
type AttemptDetail struct {
	Routing           *Routing   `json:"routing"`
	ID                string     `json:"id"`
	Ordinal           int32      `json:"ordinal"`
	ProviderID        string     `json:"provider_id"`
	ProviderName      string     `json:"provider_name"`
	UpstreamModel     string     `json:"upstream_model"`
	StartedAt         time.Time  `json:"started_at"`
	CompletedAt       *time.Time `json:"completed_at"`
	StatusCode        *int32     `json:"status_code"`
	ErrorClass        *string    `json:"error_class"`
	Committed         bool       `json:"committed"`
	LatencyMS         *int64     `json:"latency_ms"`
	FirstByteMS       *int64     `json:"first_byte_ms"`
	ChargeStatus      *string    `json:"charge_status"`
	UsageObserved     *bool      `json:"usage_observed"`
	UsageComplete     *bool      `json:"usage_complete"`
	InputTokens       *int64     `json:"input_tokens"`
	OutputTokens      *int64     `json:"output_tokens"`
	CachedInputTokens *int64     `json:"cached_input_tokens"`
	MediaUnits        *string    `json:"media_units"`
	EstimatedCost     *string    `json:"estimated_cost"`
	Currency          *string    `json:"currency"`
	Unpriced          *bool      `json:"unpriced"`
	PricingRevisionID *string    `json:"pricing_revision_id"`
}

// RequestDetail is one request with its attempt timeline.
type RequestDetail struct {
	RequestSummary
	Attempts []AttemptDetail `json:"attempts"`
}

// requestColumns projects a request with the usage its facts add up to. The
// lateral join is filtered by HAVING so a request with no facts yet reports
// null totals rather than zeros it never earned.
const requestColumns = `SELECT r.id::text, r.runtime_generation_id::text, r.api_key_id::text,
        r.route_slug, r.operation, r.surface, r.started_at, r.completed_at,
        r.status_code::int, r.error_class, r.total_latency_ms::bigint, r.first_byte_ms::bigint,
        r.attempt_count::int, u.input_tokens, u.output_tokens, u.cached_input_tokens,
        u.estimated_cost, u.currency, u.unpriced, u.usage_complete
    FROM olp_go.requests r LEFT JOIN LATERAL (
      SELECT SUM(f.input_tokens)::bigint AS input_tokens,
             SUM(f.output_tokens)::bigint AS output_tokens,
             SUM(f.cached_input_tokens)::bigint AS cached_input_tokens,
             SUM(f.estimated_cost)::text AS estimated_cost,
             btrim(MAX(f.currency)) AS currency, BOOL_OR(f.unpriced) AS unpriced,
             BOOL_AND(f.charge_status = 'not_billable' OR f.usage_complete) AS usage_complete
        FROM olp_go.attempt_usage_facts f
       WHERE f.request_id = r.id AND f.request_started_at = r.started_at
      HAVING count(*) > 0
    ) u ON true`

func (s *RequestSummary) scanTargets() []any {
	return []any{&s.ID, &s.RuntimeGenerationID, &s.APIKeyID, &s.Route, &s.Operation, &s.Surface,
		&s.StartedAt, &s.CompletedAt, &s.StatusCode, &s.ErrorClass, &s.TotalLatencyMS,
		&s.FirstByteMS, &s.AttemptCount, &s.InputTokens, &s.OutputTokens, &s.CachedInputTokens,
		&s.EstimatedCost, &s.Currency, &s.Unpriced, &s.UsageComplete}
}

// normalize pins the timestamps to UTC so the JSON always renders as Z.
func (s *RequestSummary) normalize() {
	s.StartedAt = s.StartedAt.UTC()
	if s.CompletedAt != nil {
		completed := s.CompletedAt.UTC()
		s.CompletedAt = &completed
	}
}

// ListRequests pages the request explorer newest first. The page is keyed by
// (started_at, id) rather than an offset, so rows arriving during paging cannot
// shift a reader onto a page it has already seen.
func ListRequests(ctx context.Context, q access.Queryer, f RequestFilters, cursor *Cursor, limit int) ([]RequestSummary, *string, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	var query filterQuery
	query.push(requestColumns + " WHERE true")
	f.push(&query)
	if cursor != nil {
		query.push(" AND (r.started_at, r.id) < (" + query.bind(cursor.At) + ", " + query.bind(cursor.ID) + ")")
	}
	query.push(" ORDER BY r.started_at DESC, r.id DESC LIMIT " + query.bind(int64(limit)+1))
	rows, err := q.Query(ctx, query.sql(), query.args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list requests: %w", err)
	}
	defer rows.Close()
	items := []RequestSummary{}
	for rows.Next() {
		var item RequestSummary
		if err = rows.Scan(item.scanTargets()...); err != nil {
			return nil, nil, fmt.Errorf("list requests: %w", err)
		}
		item.normalize()
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list requests: %w", err)
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		token := EncodeCursor(last.StartedAt, last.ID)
		next = &token
	}
	return items, next, nil
}

// push appends the explorer's filters. Provider and model are attempt
// properties, so they are matched with EXISTS over this request's facts.
func (f RequestFilters) push(q *filterQuery) {
	if f.Route != nil {
		q.pushBind(" AND r.route_slug = ", *f.Route)
	}
	if f.ProviderID != nil || f.Model != nil {
		q.push(" AND EXISTS (SELECT 1 FROM olp_go.attempt_usage_facts filter_fact" +
			" WHERE filter_fact.request_id = r.id AND filter_fact.request_started_at = r.started_at")
		if f.ProviderID != nil {
			q.pushBind(" AND filter_fact.provider_id = ", *f.ProviderID)
		}
		if f.Model != nil {
			q.pushBind(" AND filter_fact.upstream_model = ", *f.Model)
		}
		q.push(")")
	}
	if f.APIKey != nil {
		q.pushBind(" AND r.api_key_id = ", *f.APIKey)
	}
	if f.Operation != nil {
		q.pushBind(" AND r.operation = ", *f.Operation)
	}
	if f.StatusCode != nil {
		q.pushBind(" AND r.status_code = ", int32(*f.StatusCode))
	}
	if f.ErrorClass != nil {
		q.pushBind(" AND r.error_class = ", *f.ErrorClass)
	}
	if f.StartedAfter != nil {
		q.pushBind(" AND r.started_at >= ", *f.StartedAfter)
	}
	if f.StartedBefore != nil {
		q.pushBind(" AND r.started_at < ", *f.StartedBefore)
	}
}

const attemptColumns = `SELECT a.routing, a.id::text, a.ordinal::int, a.provider_id::text, p.name,
        a.upstream_model, a.started_at, a.completed_at, a.status_code::int, a.error_class,
        a.committed, a.latency_ms::bigint, a.first_byte_ms::bigint, f.charge_status,
        f.usage_observed, f.usage_complete, f.input_tokens, f.output_tokens, f.cached_input_tokens,
        f.media_units::text, f.estimated_cost::text, btrim(f.currency), f.unpriced,
        f.pricing_revision_id::text
    FROM olp_go.attempts a JOIN olp_go.providers p ON p.id = a.provider_id
    LEFT JOIN olp_go.attempt_usage_facts f ON f.request_id = a.request_id
        AND f.request_started_at = a.request_started_at AND f.attempt_ordinal = a.ordinal
    WHERE a.request_id = $1 AND a.request_started_at = $2 ORDER BY a.ordinal`

// GetRequest reads one request and its attempts. The request table is
// partitioned by start time, so the newest row for an identifier is the row.
// A missing request surfaces as pgx.ErrNoRows, which the HTTP layer renders as
// a 404.
func GetRequest(ctx context.Context, q access.Queryer, id string) (RequestDetail, error) {
	var detail RequestDetail
	err := q.QueryRow(ctx, requestColumns+" WHERE r.id = $1 ORDER BY r.started_at DESC LIMIT 1", id).
		Scan(detail.RequestSummary.scanTargets()...)
	if err != nil {
		return RequestDetail{}, err
	}
	detail.RequestSummary.normalize()
	rows, err := q.Query(ctx, attemptColumns, detail.ID, detail.StartedAt)
	if err != nil {
		return RequestDetail{}, fmt.Errorf("read request attempts: %w", err)
	}
	defer rows.Close()
	detail.Attempts = []AttemptDetail{}
	for rows.Next() {
		var attempt AttemptDetail
		var routing []byte
		if err = rows.Scan(&routing, &attempt.ID, &attempt.Ordinal, &attempt.ProviderID,
			&attempt.ProviderName, &attempt.UpstreamModel, &attempt.StartedAt, &attempt.CompletedAt,
			&attempt.StatusCode, &attempt.ErrorClass, &attempt.Committed, &attempt.LatencyMS,
			&attempt.FirstByteMS, &attempt.ChargeStatus, &attempt.UsageObserved,
			&attempt.UsageComplete, &attempt.InputTokens, &attempt.OutputTokens,
			&attempt.CachedInputTokens, &attempt.MediaUnits, &attempt.EstimatedCost,
			&attempt.Currency, &attempt.Unpriced, &attempt.PricingRevisionID); err != nil {
			return RequestDetail{}, fmt.Errorf("read request attempts: %w", err)
		}
		if routing != nil {
			// Stored provenance is re-decoded rather than passed through, so a
			// row written by an older or damaged writer cannot reach the
			// console as a shape the contract does not describe.
			var parsed Routing
			if err = json.Unmarshal(routing, &parsed); err != nil {
				return RequestDetail{}, fmt.Errorf("read request attempts: stored routing is invalid")
			}
			attempt.Routing = &parsed
		}
		attempt.StartedAt = attempt.StartedAt.UTC()
		if attempt.CompletedAt != nil {
			completed := attempt.CompletedAt.UTC()
			attempt.CompletedAt = &completed
		}
		detail.Attempts = append(detail.Attempts, attempt)
	}
	if err = rows.Err(); err != nil {
		return RequestDetail{}, fmt.Errorf("read request attempts: %w", err)
	}
	return detail, nil
}

// Validate refuses a filter set that cannot describe any request. An impossible
// window or an unknown operation is an error, not an empty page: the caller
// would otherwise read "no traffic" from a typo.
func (f RequestFilters) Validate() error {
	if f.StartedAfter != nil && f.StartedBefore != nil && !f.StartedBefore.After(*f.StartedAfter) {
		return access.Fail(400, "invalid_range", "Use a positive started_after to started_before range.")
	}
	if f.Operation != nil && !validOperation(*f.Operation) {
		return access.Fail(400, "invalid_operation", "The operation filter is invalid.")
	}
	return nil
}
