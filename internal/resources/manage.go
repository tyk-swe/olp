package resources

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oapi-codegen/nullable"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
)

type Management struct {
	Access *access.Server
	Pool   *pgxpool.Pool
}

func (m *Management) Register(mux *http.ServeMux) {
	m.registerCodeMode(mux)
	m.Access.Route(mux, "GET /api/v1/provider-resources", m.list)
}

type manageFilters struct {
	allProjects bool
	projects    []string
	kind        *string
	apiKeyID    *string
	providerID  *string
	route       *string
	state       *string
}

type manageCursor struct {
	at time.Time
	id string
}

func (c *manageCursor) encode() string {
	return c.at.UTC().Format("2006-01-02T15:04:05.999999999Z07:00") + "|" + c.id
}

func decodeManageCursor(value string) (*manageCursor, error) {
	at, id, ok := strings.Cut(value, "|")
	if !ok {
		return nil, errors.New("invalid cursor")
	}
	parsed, err := time.Parse("2006-01-02T15:04:05.999999999Z07:00", at)
	if err != nil {
		return nil, errors.New("invalid cursor")
	}
	if _, err = uuid.Parse(id); err != nil {
		return nil, errors.New("invalid cursor")
	}
	return &manageCursor{at: parsed, id: id}, nil
}

type manageRecord struct {
	uuid          uuid.UUID
	kind          string
	apiKeyID      string
	apiKeyName    string
	route         string
	providerID    string
	providerName  string
	upstreamID    string
	upstreamModel string
	state         string
	expiresAt     *time.Time
	createdAt     time.Time
	updatedAt     time.Time
	settlement    *manageSettlementRecord
}

type manageSettlementRecord struct {
	state            string
	expectedResults  *int64
	settledResults   int64
	uncertainResults int64
	lastErrorClass   *string
	closeAfter       time.Time
	closedAt         *time.Time
}

type providerResourceSettlement = struct {
	CloseAfter       time.Time                    `json:"close_after"`
	ClosedAt         nullable.Nullable[time.Time] `json:"closed_at"`
	ExpectedResults  nullable.Nullable[int64]     `json:"expected_results"`
	LastErrorClass   nullable.Nullable[string]    `json:"last_error_class"`
	SettledResults   int64                        `json:"settled_results"`
	State            string                       `json:"state"`
	UncertainResults int64                        `json:"uncertain_results"`
}

func (m *Management) list(r *http.Request, p access.Principal) (access.Reply, error) {
	query := r.URL.Query()
	filters := manageFilters{allProjects: p.AllProjects, projects: p.ProjectIDs()}
	if raw := query.Get("kind"); raw != "" {
		if !contract.ListProviderResourcesParamsKind(raw).Valid() {
			return access.Reply{}, access.Invalid("kind",
				"Use file, batch, response, continuation, strict_response, interaction, strict_file, strict_batch, anthropic_file, anthropic_batch, gemini_upload, gemini_file, gemini_batch, or cloud_batch.")
		}
		filters.kind = &raw
	}
	if raw := query.Get("api_key_id"); raw != "" {
		if _, err := uuid.Parse(raw); err != nil {
			return access.Reply{}, access.Invalid("api_key_id", "Use an API key identifier.")
		}
		filters.apiKeyID = &raw
	}
	if raw := query.Get("provider_id"); raw != "" {
		if _, err := uuid.Parse(raw); err != nil {
			return access.Reply{}, access.Invalid("provider_id", "Use a provider identifier.")
		}
		filters.providerID = &raw
	}
	if raw := query.Get("route"); raw != "" {
		filters.route = &raw
	}
	if raw := query.Get("state"); raw != "" {
		filters.state = &raw
	}
	limit := 50
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			return access.Reply{}, access.Invalid("limit", "Page size must be between 1 and 200.")
		}
		limit = parsed
	}
	var cursor *manageCursor
	if raw := query.Get("cursor"); raw != "" {
		decoded, err := decodeManageCursor(raw)
		if err != nil {
			return access.Reply{}, access.Fail(http.StatusBadRequest, "invalid_cursor", "The page cursor is malformed; restart the listing.")
		}
		cursor = decoded
	}
	records, next, err := m.page(r.Context(), filters, cursor, limit)
	if err != nil {
		return access.Reply{}, err
	}
	items := make([]contract.ProviderResourceItem, len(records))
	for i, record := range records {
		item := contract.ProviderResourceItem{
			Id:            LocalID(record.kind, record.uuid),
			Kind:          contract.ProviderResourceItemKind(record.kind),
			ApiKeyId:      uuid.MustParse(record.apiKeyID),
			ApiKeyName:    record.apiKeyName,
			Route:         record.route,
			ProviderId:    uuid.MustParse(record.providerID),
			ProviderName:  record.providerName,
			UpstreamId:    record.upstreamID,
			UpstreamModel: record.upstreamModel,
			State:         record.state,
			CreatedAt:     record.createdAt,
			UpdatedAt:     record.updatedAt,
		}
		if record.expiresAt != nil {
			item.ExpiresAt = nullable.NewNullableWithValue(*record.expiresAt)
		}
		if record.settlement != nil {
			var expected nullable.Nullable[int64]
			if record.settlement.expectedResults != nil {
				expected = nullable.NewNullableWithValue(*record.settlement.expectedResults)
			}
			var lastError nullable.Nullable[string]
			if record.settlement.lastErrorClass != nil {
				lastError = nullable.NewNullableWithValue(*record.settlement.lastErrorClass)
			}
			var closed nullable.Nullable[time.Time]
			if record.settlement.closedAt != nil {
				closed = nullable.NewNullableWithValue(*record.settlement.closedAt)
			}
			item.Settlement = nullable.NewNullableWithValue(providerResourceSettlement{
				State: record.settlement.state, ExpectedResults: expected,
				SettledResults: record.settlement.settledResults, UncertainResults: record.settlement.uncertainResults,
				LastErrorClass: lastError, CloseAfter: record.settlement.closeAfter, ClosedAt: closed,
			})
		}
		items[i] = item
	}
	response := contract.ProviderResourceListResponse{Items: items}
	if next != nil {
		response.NextCursor = nullable.NewNullableWithValue(*next)
	}
	return access.OK(response), nil
}

func (m *Management) page(ctx context.Context, filters manageFilters, cursor *manageCursor, limit int) ([]manageRecord, *string, error) {
	query := `WITH entries AS (
		SELECT r.id, r.kind, r.api_key_id, k.name AS api_key_name, r.route_slug AS route,
			r.provider_id, p.name AS provider_name, r.upstream_id,
			COALESCE(r.metadata->>'upstream_model', '') AS upstream_model, r.state,
			r.expires_at, r.created_at, r.updated_at,
			bs.resource_id AS settlement_id, bs.state AS settlement_state, bs.expected_results,
			bs.settled_results, bs.uncertain_results, bs.last_error_class, bs.close_after, bs.closed_at
		FROM olp.provider_resources r
		JOIN olp.providers p ON p.id = r.provider_id
		JOIN olp.api_keys k ON k.id = r.api_key_id
		LEFT JOIN olp.batch_settlements bs ON bs.resource_id = r.id
		UNION ALL
		SELECT bs.resource_id AS id, bs.kind, bs.api_key_id, k.name AS api_key_name,
			bs.route_slug AS route, bs.provider_id, p.name AS provider_name,
			COALESCE(bs.upstream_id, '') AS upstream_id,
			COALESCE(bs.template->>'upstream_model', '') AS upstream_model, bs.state,
			bs.close_after AS expires_at, bs.created_at, bs.updated_at,
			bs.resource_id AS settlement_id, bs.state AS settlement_state, bs.expected_results,
			bs.settled_results, bs.uncertain_results, bs.last_error_class, bs.close_after, bs.closed_at
		FROM olp.batch_settlements bs
		LEFT JOIN olp.provider_resources r ON r.id = bs.resource_id
		JOIN olp.providers p ON p.id = bs.provider_id
		JOIN olp.api_keys k ON k.id = bs.api_key_id
		WHERE r.id IS NULL AND bs.state IN ('submitting', 'submission_unknown')
	)
	SELECT e.id, e.kind, e.api_key_id::text, e.api_key_name, e.route, e.provider_id::text, e.provider_name,
		e.upstream_id, e.upstream_model, e.state, e.expires_at, e.created_at, e.updated_at,
		e.settlement_id, e.settlement_state, e.expected_results, e.settled_results,
		e.uncertain_results, e.last_error_class, e.close_after, e.closed_at
	FROM entries e WHERE TRUE`
	var args []any
	push := func(clause string, value any) {
		args = append(args, value)
		query += fmt.Sprintf(clause, len(args))
	}
	if !filters.allProjects {
		push(" AND e.api_key_id IN (SELECT id FROM olp.api_keys WHERE project_id = ANY($%d::uuid[]))", filters.projects)
	}
	if filters.kind != nil {
		push(" AND e.kind = $%d", *filters.kind)
	}
	if filters.apiKeyID != nil {
		push(" AND e.api_key_id = $%d", *filters.apiKeyID)
	}
	if filters.providerID != nil {
		push(" AND e.provider_id = $%d", *filters.providerID)
	}
	if filters.route != nil {
		push(" AND e.route = $%d", *filters.route)
	}
	if filters.state != nil {
		push(" AND e.state = $%d", *filters.state)
	}
	if cursor != nil {
		args = append(args, cursor.at, cursor.id)
		query += fmt.Sprintf(" AND (e.created_at, e.id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)
	query += fmt.Sprintf(" ORDER BY e.created_at DESC, e.id DESC LIMIT $%d", len(args))
	rows, err := m.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var records []manageRecord
	for rows.Next() {
		var record manageRecord
		var settlementID *uuid.UUID
		var settlementState *string
		var expectedResults *int64
		var settledResults, uncertainResults *int64
		var lastErrorClass *string
		var closeAfter, closedAt *time.Time
		if err = rows.Scan(&record.uuid, &record.kind, &record.apiKeyID, &record.apiKeyName,
			&record.route, &record.providerID, &record.providerName, &record.upstreamID,
			&record.upstreamModel, &record.state, &record.expiresAt,
			&record.createdAt, &record.updatedAt, &settlementID, &settlementState, &expectedResults,
			&settledResults, &uncertainResults, &lastErrorClass, &closeAfter, &closedAt); err != nil {
			return nil, nil, err
		}
		if settlementID != nil && settlementState != nil && closeAfter != nil {
			record.settlement = &manageSettlementRecord{
				state: *settlementState, expectedResults: expectedResults, settledResults: valueOrZero(settledResults),
				uncertainResults: valueOrZero(uncertainResults), lastErrorClass: lastErrorClass,
				closeAfter: *closeAfter, closedAt: closedAt,
			}
		}
		records = append(records, record)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *string
	if len(records) > limit {
		last := records[limit-1]
		encoded := (&manageCursor{at: last.createdAt, id: last.uuid.String()}).encode()
		next = &encoded
		records = records[:limit]
	}
	return records, next, nil
}

func valueOrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
