package media

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oapi-codegen/nullable"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
)

// Management serves the operator-facing media-job inspection surface. Every
// response is metadata-only: no prompts, payloads, or provider bodies.
type Management struct {
	Access *access.Server
	Pool   *pgxpool.Pool
}

// Register mounts the media-job routes on the management surface.
func (m *Management) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v3/media-jobs", m.Access.Handle(m.list))
	mux.HandleFunc("GET /api/v3/media-jobs/{job_id}", m.Access.Handle(m.get))
}

func (m *Management) list(r *http.Request) (access.Reply, error) {
	if _, err := m.Access.Principal(r, m.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	query := r.URL.Query()
	filters := Filters{}
	if raw := query.Get("api_key_id"); raw != "" {
		if _, err := uuid.Parse(raw); err != nil {
			return access.Reply{}, access.Invalid("api_key_id", "Use an API key identifier.")
		}
		filters.APIKeyID = &raw
	}
	if raw := query.Get("provider_id"); raw != "" {
		if _, err := uuid.Parse(raw); err != nil {
			return access.Reply{}, access.Invalid("provider_id", "Use a provider identifier.")
		}
		filters.ProviderID = &raw
	}
	if raw := query.Get("route"); raw != "" {
		filters.RouteSlug = &raw
	}
	if raw := query.Get("state"); raw != "" {
		state := State(raw)
		switch state {
		case StateQueued, StateRunning, StateSucceeded, StateFailed, StateCancelled:
		default:
			return access.Reply{}, access.Invalid("state", "State must be queued, running, succeeded, failed, or cancelled.")
		}
		filters.State = &state
	}
	if raw := query.Get("lifecycle"); raw != "" {
		lifecycle := Lifecycle(raw)
		switch lifecycle {
		case LifecycleCreating, LifecycleActive, LifecycleCreateAmbiguous, LifecycleCreateCleanupPending, LifecycleDeletePending, LifecycleDeleted:
		default:
			return access.Reply{}, access.Invalid("lifecycle", "Unknown media-job reconciliation lifecycle.")
		}
		filters.Lifecycle = &lifecycle
	}
	if raw := query.Get("created_after"); raw != "" {
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return access.Reply{}, access.Invalid("created_after", "Use an RFC 3339 timestamp.")
		}
		filters.CreatedAfter = &at
	}
	if raw := query.Get("created_before"); raw != "" {
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return access.Reply{}, access.Invalid("created_before", "Use an RFC 3339 timestamp.")
		}
		filters.CreatedBefore = &at
	}
	if filters.CreatedAfter != nil && filters.CreatedBefore != nil && !filters.CreatedAfter.Before(*filters.CreatedBefore) {
		return access.Reply{}, access.Invalid("created_before", "created_before must be after created_after.")
	}
	limit := 50
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			return access.Reply{}, access.Invalid("limit", "Page size must be between 1 and 200.")
		}
		limit = parsed
	}
	var cursor *Cursor
	if raw := query.Get("cursor"); raw != "" {
		decoded, err := DecodeCursor(raw)
		if err != nil {
			return access.Reply{}, access.Fail(http.StatusBadRequest, "invalid_cursor", "The page cursor is malformed; restart the listing.")
		}
		cursor = decoded
	}
	page, err := Jobs(r.Context(), m.Pool, filters, cursor, limit)
	if err != nil {
		return access.Reply{}, mapJobError(err)
	}
	items := make([]contract.MediaJobItem, len(page.Items))
	for i, record := range page.Items {
		items[i] = jobItem(&record)
	}
	response := contract.MediaJobListResponse{Items: items}
	if page.NextCursor != nil {
		response.NextCursor = nullable.NewNullableWithValue(*page.NextCursor)
	}
	return access.OK(response), nil
}

func (m *Management) get(r *http.Request) (access.Reply, error) {
	if _, err := m.Access.Principal(r, m.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	if _, err := uuid.Parse(r.PathValue("job_id")); err != nil {
		return access.Reply{}, access.Fail(http.StatusNotFound, "not_found", "The media job does not exist.")
	}
	record, err := Job(r.Context(), m.Pool, r.PathValue("job_id"))
	if err != nil {
		return access.Reply{}, mapJobError(err)
	}
	return access.Detail(jobItem(&record), record.ETag), nil
}

// jobItem renders one record in the management wire shape.
func jobItem(record *JobRecord) contract.MediaJobItem {
	item := contract.MediaJobItem{
		Id:               uuid.MustParse(record.ID),
		ApiKeyId:         uuid.MustParse(record.APIKeyID),
		ProviderId:       uuid.MustParse(record.ProviderID),
		ProviderName:     record.ProviderName,
		ProviderModel:    record.UpstreamModel,
		Route:            record.RouteSlug,
		Operation:        record.Operation,
		Surface:          record.Surface,
		State:            string(record.State),
		Lifecycle:        string(record.Lifecycle),
		ContentAvailable: record.ContentAvailable,
		Etag:             record.ETag,
		CreatedAt:        record.CreatedAt,
		UpdatedAt:        record.UpdatedAt,
	}
	if record.UpstreamJobID != nil {
		item.UpstreamJobId = nullable.NewNullableWithValue(*record.UpstreamJobID)
	}
	if record.ProgressPercent != nil {
		item.ProgressPercent = nullable.NewNullableWithValue(*record.ProgressPercent)
	}
	if record.ExpiresAt != nil {
		item.ExpiresAt = nullable.NewNullableWithValue(*record.ExpiresAt)
	}
	if record.ErrorClass != nil {
		item.ErrorClass = nullable.NewNullableWithValue(*record.ErrorClass)
	}
	if record.CompletedAt != nil {
		item.CompletedAt = nullable.NewNullableWithValue(*record.CompletedAt)
	}
	if record.LastPolledAt != nil {
		item.LastPolledAt = nullable.NewNullableWithValue(*record.LastPolledAt)
	}
	if record.ReconciliationError != nil {
		item.ReconciliationError = nullable.NewNullableWithValue(*record.ReconciliationError)
	}
	if record.DeletedAt != nil {
		item.DeletedAt = nullable.NewNullableWithValue(*record.DeletedAt)
	}
	return item
}

// mapJobError translates durable job errors into management problems.
func mapJobError(err error) error {
	var jobErr *JobError
	if errors.As(err, &jobErr) {
		switch jobErr.Kind {
		case JobErrorNotFound:
			return access.Fail(http.StatusNotFound, "not_found", "The media job does not exist.")
		case JobErrorPrecondition:
			return access.Fail(http.StatusPreconditionFailed, "etag_mismatch", "The media job changed; refresh it and retry with the current ETag.")
		case JobErrorUpstreamIdentityConflict:
			return access.Fail(http.StatusConflict, "media_job_upstream_identity_conflict", "The upstream media job is already bound to different metadata.")
		case JobErrorInvalid:
			return access.Invalid("media_job", jobErr.Message)
		}
	}
	return err
}
