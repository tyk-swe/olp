package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

func (a *API) contracts(_ *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return httpkit.OK(buildContracts()), nil
}

func buildContracts() contract.ProviderResourceContractList {
	families := retainedapi.Families()
	items := make([]contract.ProviderResourceContract, len(families))
	for i, family := range families {
		implemented := slices.ContainsFunc(family.Providers, func(provider string) bool {
			return retainedapi.Supports(provider, family.Operation, family.Surface, "unary")
		})
		items[i] = contract.ProviderResourceContract{
			Operation:   family.Operation,
			Surface:     family.Surface,
			Kind:        family.Kind,
			Path:        family.Path,
			Methods:     ptr.NonNil(slices.Clone(family.Methods)),
			Providers:   ptr.NonNil(slices.Clone(family.Providers)),
			Implemented: implemented,
			Metered:     family.Metered,
		}
	}
	rawEndpoints := retainedapi.Endpoints()
	endpoints := make([]contract.ProviderResourceEndpoint, len(rawEndpoints))
	for i, e := range rawEndpoints {
		endpoints[i] = contract.ProviderResourceEndpoint{
			Operation: e.Operation,
			Surface:   e.Surface,
			Kind:      e.Kind,
			Action:    e.Action,
			Method:    e.Method,
			Pattern:   e.Pattern,
			Ancestors: ptr.NonNil(slices.Clone(e.Ancestors)),
			Metered:   e.Metered,
		}
	}
	return contract.ProviderResourceContractList{
		Revision:  retainedapi.Revision,
		Items:     items,
		Endpoints: &endpoints,
	}
}

type listFilters struct {
	allProjects bool
	projects    []string
	kind        *string
	apiKeyID    *uuid.UUID
	providerID  *uuid.UUID
	route       *string
	state       *string
}

type listCursor struct {
	at time.Time
	id uuid.UUID
}

func (c listCursor) encode() string {
	return c.at.UTC().Format(time.RFC3339Nano) + "|" + c.id.String()
}

func decodeListCursor(value string) (*listCursor, error) {
	at, rawID, ok := strings.Cut(value, "|")
	if !ok {
		return nil, errors.New("invalid cursor")
	}
	parsedAt, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, errors.New("invalid cursor")
	}
	parsedID, err := uuid.Parse(rawID)
	if err != nil {
		return nil, errors.New("invalid cursor")
	}
	return &listCursor{at: parsedAt, id: parsedID}, nil
}

type listQuery struct {
	filters listFilters
	cursor  *listCursor
	limit   int
}

func parseListQuery(r *http.Request, p authz.Principal) (listQuery, error) {
	query := r.URL.Query()
	filters := listFilters{allProjects: p.AllProjects, projects: p.ProjectIDs()}
	if raw := query.Get("kind"); raw != "" {
		if !contract.ListProviderResourcesParamsKind(raw).Valid() {
			return listQuery{}, problem.Invalid("kind", "Use a resource kind from the provider resource contract catalogue.")
		}
		filters.kind = &raw
	}
	if raw := query.Get("api_key_id"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			return listQuery{}, problem.Invalid("api_key_id", "Use an API key identifier.")
		}
		filters.apiKeyID = &parsed
	}
	if raw := query.Get("provider_id"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			return listQuery{}, problem.Invalid("provider_id", "Use a provider identifier.")
		}
		filters.providerID = &parsed
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
			return listQuery{}, problem.Invalid("limit", "Page size must be between 1 and 200.")
		}
		limit = parsed
	}
	var cursor *listCursor
	if raw := query.Get("cursor"); raw != "" {
		decoded, err := decodeListCursor(raw)
		if err != nil {
			return listQuery{}, problem.Fail(http.StatusBadRequest, "invalid_cursor", "The page cursor is malformed; restart the listing.")
		}
		cursor = decoded
	}
	return listQuery{filters: filters, cursor: cursor, limit: limit}, nil
}

type listRecord struct {
	uuid          uuid.UUID
	kind          string
	apiKeyID      uuid.UUID
	apiKeyName    string
	route         string
	providerID    uuid.UUID
	providerName  string
	upstreamID    string
	upstreamModel string
	state         string
	expiresAt     *time.Time
	createdAt     time.Time
	updatedAt     time.Time
	retained      *contract.RetainedSettlementStatus
	settlement    *settlementRecord
}

type settlementRecord struct {
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

func (a *API) list(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	q, err := parseListQuery(r, p)
	if err != nil {
		return httpkit.Reply{}, err
	}
	records, next, err := a.listPage(r.Context(), q)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items := make([]contract.ProviderResourceItem, len(records))
	for i, record := range records {
		items[i] = toProviderResourceItem(record)
	}
	return httpkit.OK(contract.ProviderResourceListResponse{
		Items:      items,
		NextCursor: nullableValue(next),
	}), nil
}

func toProviderResourceItem(record listRecord) contract.ProviderResourceItem {
	item := contract.ProviderResourceItem{
		Id:                 resources.LocalID(record.kind, record.uuid),
		Kind:               contract.ProviderResourceItemKind(record.kind),
		ApiKeyId:           record.apiKeyID,
		ApiKeyName:         record.apiKeyName,
		Route:              record.route,
		ProviderId:         record.providerID,
		ProviderName:       record.providerName,
		UpstreamId:         record.upstreamID,
		UpstreamModel:      record.upstreamModel,
		State:              record.state,
		ExpiresAt:          nullableValue(record.expiresAt),
		CreatedAt:          record.createdAt,
		UpdatedAt:          record.updatedAt,
		RetainedSettlement: record.retained,
	}
	if record.settlement != nil {
		item.Settlement = nullable.NewNullableWithValue(providerResourceSettlement{
			State:            record.settlement.state,
			ExpectedResults:  nullableValue(record.settlement.expectedResults),
			SettledResults:   record.settlement.settledResults,
			UncertainResults: record.settlement.uncertainResults,
			LastErrorClass:   nullableValue(record.settlement.lastErrorClass),
			CloseAfter:       record.settlement.closeAfter,
			ClosedAt:         nullableValue(record.settlement.closedAt),
		})
	}
	return item
}

func (a *API) listPage(ctx context.Context, q listQuery) ([]listRecord, *string, error) {
	sqlQuery, args := listPageQuery(q)
	rows, err := a.server.Pool.Query(ctx, sqlQuery, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	records := make([]listRecord, 0, q.limit)
	for rows.Next() {
		record, err := scanListRecord(rows)
		if err != nil {
			return nil, nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(records) <= q.limit {
		return records, nil, nil
	}
	last := records[q.limit-1]
	next := listCursor{at: last.createdAt, id: last.uuid}.encode()
	return records[:q.limit], &next, nil
}

func scanListRecord(row pgx.Row) (listRecord, error) {
	var record listRecord
	var retainedRaw []byte
	var settlementID *uuid.UUID
	var settlementState, lastErrorClass *string
	var expectedResults, settledResults, uncertainResults *int64
	var closeAfter, closedAt *time.Time
	err := row.Scan(
		&record.uuid, &record.kind, &record.apiKeyID, &record.apiKeyName,
		&record.route, &record.providerID, &record.providerName, &record.upstreamID,
		&record.upstreamModel, &record.state, &record.expiresAt,
		&record.createdAt, &record.updatedAt, &settlementID, &settlementState, &expectedResults,
		&settledResults, &uncertainResults, &lastErrorClass, &closeAfter, &closedAt, &retainedRaw,
	)
	if err != nil {
		return listRecord{}, err
	}
	if len(retainedRaw) > 0 && string(retainedRaw) != "null" {
		var status contract.RetainedSettlementStatus
		if err := json.Unmarshal(retainedRaw, &status); err != nil {
			return listRecord{}, err
		}
		record.retained = &status
	}
	if settlementID != nil && settlementState != nil && closeAfter != nil {
		record.settlement = &settlementRecord{
			state:            *settlementState,
			expectedResults:  expectedResults,
			settledResults:   ptr.Value(settledResults),
			uncertainResults: ptr.Value(uncertainResults),
			lastErrorClass:   lastErrorClass,
			closeAfter:       *closeAfter,
			closedAt:         closedAt,
		}
	}
	return record, nil
}

func listPageQuery(q listQuery) (string, []any) {
	var b strings.Builder
	b.WriteString(`WITH entries AS (
		SELECT r.id, r.kind, r.api_key_id, k.name AS api_key_name, r.route_slug AS route,
			r.provider_id, p.name AS provider_name, r.upstream_id,
			COALESCE(r.metadata->>'upstream_model', '') AS upstream_model, r.state,
			r.expires_at, r.created_at, r.updated_at,
			bs.resource_id AS settlement_id, bs.state AS settlement_state, bs.expected_results,
			bs.settled_results, bs.uncertain_results, bs.last_error_class, bs.close_after, bs.closed_at,
			CASE WHEN rs.resource_id IS NULL THEN NULL ELSE jsonb_build_object(
				'state', rs.state, 'missing_evidence', rs.state <> 'closed',
				'remote_terminated', rs.remote_terminated, 'terminal_at', rs.terminal_at,
				'last_error', rs.last_error, 'next_observation_at', rs.poll_after,
				'models', ARRAY(SELECT jsonb_object_keys(rs.templates))
			) END AS retained_settlement
		FROM olp.provider_resources r
		JOIN olp.providers p ON p.id = r.provider_id
		JOIN olp.api_keys k ON k.id = r.api_key_id
		LEFT JOIN olp.batch_settlements bs ON bs.resource_id = r.id
		LEFT JOIN olp.retained_settlements rs ON rs.resource_id = r.id
		UNION ALL
		SELECT bs.resource_id AS id, bs.kind, bs.api_key_id, k.name AS api_key_name,
			bs.route_slug AS route, bs.provider_id, p.name AS provider_name,
			COALESCE(bs.upstream_id, '') AS upstream_id,
			COALESCE(bs.template->>'upstream_model', '') AS upstream_model, bs.state,
			bs.close_after AS expires_at, bs.created_at, bs.updated_at,
			bs.resource_id AS settlement_id, bs.state AS settlement_state, bs.expected_results,
			bs.settled_results, bs.uncertain_results, bs.last_error_class, bs.close_after, bs.closed_at,
			NULL::jsonb AS retained_settlement
		FROM olp.batch_settlements bs
		LEFT JOIN olp.provider_resources r ON r.id = bs.resource_id
		JOIN olp.providers p ON p.id = bs.provider_id
		JOIN olp.api_keys k ON k.id = bs.api_key_id
		WHERE r.id IS NULL AND bs.state IN ('submitting', 'submission_unknown', 'conflict', 'uncertain')
	)
	SELECT e.id, e.kind, e.api_key_id, e.api_key_name, e.route, e.provider_id, e.provider_name,
		e.upstream_id, e.upstream_model, e.state, e.expires_at, e.created_at, e.updated_at,
		e.settlement_id, e.settlement_state, e.expected_results, e.settled_results,
		e.uncertain_results, e.last_error_class, e.close_after, e.closed_at, e.retained_settlement
	FROM entries e WHERE TRUE`)

	var args []any
	push := func(clause string, value any) {
		args = append(args, value)
		fmt.Fprintf(&b, clause, len(args))
	}
	filters := q.filters
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
	if q.cursor != nil {
		args = append(args, q.cursor.at, q.cursor.id)
		fmt.Fprintf(&b, " AND (e.created_at, e.id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, q.limit+1)
	fmt.Fprintf(&b, " ORDER BY e.created_at DESC, e.id DESC LIMIT $%d", len(args))
	return b.String(), args
}

func nullableValue[T any](v *T) nullable.Nullable[T] {
	if v == nil {
		return nullable.NewNullNullable[T]()
	}
	return nullable.NewNullableWithValue(*v)
}
