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
	mux.HandleFunc("GET /api/v3/provider-resources", m.Access.Handle(m.list))
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
}

func (m *Management) list(r *http.Request) (access.Reply, error) {
	p, err := m.Access.Principal(r, m.Pool, "read")
	if err != nil {
		return access.Reply{}, err
	}
	query := r.URL.Query()
	filters := manageFilters{allProjects: p.AllProjects, projects: p.ProjectIDs()}
	if raw := query.Get("kind"); raw != "" {
		switch raw {
		case KindFile, KindBatch, KindResponse:
		default:
			return access.Reply{}, access.Invalid("kind", "Kind must be file, batch, or response.")
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
		items[i] = item
	}
	response := contract.ProviderResourceListResponse{Items: items}
	if next != nil {
		response.NextCursor = nullable.NewNullableWithValue(*next)
	}
	return access.OK(response), nil
}

func (m *Management) page(ctx context.Context, filters manageFilters, cursor *manageCursor, limit int) ([]manageRecord, *string, error) {
	query := `SELECT r.id, r.kind, r.api_key_id::text, k.name, r.route_slug,
		r.provider_id::text, p.name, r.upstream_id,
		COALESCE(r.metadata->>'upstream_model', ''), r.state,
		r.expires_at, r.created_at, r.updated_at
		FROM olp_go.provider_resources r
		JOIN olp_go.providers p ON p.id = r.provider_id
		JOIN olp_go.api_keys k ON k.id = r.api_key_id
		WHERE TRUE`
	var args []any
	push := func(clause string, value any) {
		args = append(args, value)
		query += fmt.Sprintf(clause, len(args))
	}
	if !filters.allProjects {
		push(" AND r.api_key_id IN (SELECT id FROM olp_go.api_keys WHERE project_id = ANY($%d::uuid[]))", filters.projects)
	}
	if filters.kind != nil {
		push(" AND r.kind = $%d", *filters.kind)
	}
	if filters.apiKeyID != nil {
		push(" AND r.api_key_id = $%d", *filters.apiKeyID)
	}
	if filters.providerID != nil {
		push(" AND r.provider_id = $%d", *filters.providerID)
	}
	if filters.route != nil {
		push(" AND r.route_slug = $%d", *filters.route)
	}
	if filters.state != nil {
		push(" AND r.state = $%d", *filters.state)
	}
	if cursor != nil {
		args = append(args, cursor.at, cursor.id)
		query += fmt.Sprintf(" AND (r.created_at, r.id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)
	query += fmt.Sprintf(" ORDER BY r.created_at DESC, r.id DESC LIMIT $%d", len(args))
	rows, err := m.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var records []manageRecord
	for rows.Next() {
		var record manageRecord
		if err = rows.Scan(&record.uuid, &record.kind, &record.apiKeyID, &record.apiKeyName,
			&record.route, &record.providerID, &record.providerName, &record.upstreamID,
			&record.upstreamModel, &record.state, &record.expiresAt,
			&record.createdAt, &record.updatedAt); err != nil {
			return nil, nil, err
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
