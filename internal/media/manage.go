package media

import (
	"errors"
	"io"
	"log/slog"
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

// Management serves the operator-facing media-job inspection surface. Every
// response is metadata-only: no prompts, payloads, or provider bodies.
type Management struct {
	Access *access.Server
	Pool   *pgxpool.Pool

	Jobs *Service
	Log  *slog.Logger
}

const operatorJobTimeout = 5 * time.Minute

// Register mounts the media-job routes on the management surface.
func (m *Management) Register(mux *http.ServeMux) {
	m.Access.Route(mux, "GET /api/v1/media-jobs", m.list)
	m.Access.Route(mux, "GET /api/v1/media-jobs/{job_id}", m.get)
	m.Access.Route(mux, "POST /api/v1/media-jobs/{job_id}/refresh", m.refresh, access.MaxBody(1024), access.Deadline(operatorJobTimeout))
	m.Access.Stream(mux, "GET /api/v1/media-jobs/{job_id}/content", m.content, access.MaxBody(1024), access.Deadline(operatorJobTimeout))
	m.Access.Route(mux, "DELETE /api/v1/media-jobs/{job_id}", m.delete, access.MaxBody(1024), access.Deadline(operatorJobTimeout))
}

func (m *Management) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}

func (m *Management) scopedJob(r *http.Request, q Querier, p access.Principal, need access.Need) (*JobRecord, error) {
	if _, err := uuid.Parse(r.PathValue("job_id")); err != nil {
		return nil, access.Fail(http.StatusNotFound, "not_found", "The media job does not exist.")
	}
	record, err := Job(r.Context(), q, r.PathValue("job_id"))
	if err != nil {
		return nil, mapJobError(err)
	}
	var keyProject *string
	if err = q.QueryRow(r.Context(), "SELECT project_id::text FROM olp.api_keys WHERE id=$1", record.APIKeyID).Scan(&keyProject); err != nil {
		return nil, mapJobError(err)
	}
	if err := p.Project(keyProject, need); err != nil {
		return nil, err
	}
	return &record, nil
}

// reauthorizeJob resolves the caller and the job's project again under the
// installation lock, so a job mutation never starts under authority revoked
// since admission. The lock is released before the provider call runs.
func (m *Management) reauthorizeJob(r *http.Request, need access.Need) (access.Principal, *JobRecord, error) {
	tx, err := m.Access.Begin(r)
	if err != nil {
		return access.Principal{}, nil, err
	}
	defer tx.Rollback(r.Context())
	p, err := m.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Principal{}, nil, err
	}
	record, err := m.scopedJob(r, tx, p, need)
	if err != nil {
		return access.Principal{}, nil, err
	}
	return p, record, nil
}

func (m *Management) jobsAvailable() error {
	if m.Jobs == nil || m.Jobs.Pool == nil {
		return access.Fail(http.StatusServiceUnavailable, "media_job_unavailable", "Media job reconciliation is not configured on this process.")
	}
	return nil
}

func (m *Management) audit(r *http.Request, p access.Principal, action, id string) error {
	tx, err := m.Pool.Begin(r.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(r.Context())
	if err := access.Audit(r.Context(), tx, r, p.Actor(), action, "media_job", id, "success"); err != nil {
		return err
	}
	return tx.Commit(r.Context())
}

func (m *Management) refresh(r *http.Request, _ access.Principal) (access.Reply, error) {
	p, record, err := m.reauthorizeJob(r, access.Change)
	if err != nil {
		return access.Reply{}, err
	}
	if err := m.jobsAvailable(); err != nil {
		return access.Reply{}, err
	}
	updated, err := m.Jobs.RefreshJob(r.Context(), record.ID)
	if err != nil {
		return access.Reply{}, mapJobError(err)
	}
	if err := m.audit(r, p, "media_job.refresh", updated.ID); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(jobItem(updated), updated.ETag), nil
}

func (m *Management) content(w http.ResponseWriter, r *http.Request, p access.Principal) error {
	record, err := m.scopedJob(r, m.Pool, p, access.Change)
	if err != nil {
		return err
	}
	variant, failure := ValidateVideoContentQuery(r.URL.Query())
	if failure != nil {
		return access.Fail(failure.Status, failure.Code, failure.Message)
	}
	if err := m.jobsAvailable(); err != nil {
		return err
	}
	result, err := m.Jobs.JobContent(r.Context(), record, variant)
	if errors.Is(err, ErrContentUnavailable) {
		return access.Fail(http.StatusConflict, "media_content_unavailable", "The media job has no downloadable "+variant+" content.")
	}
	if err != nil {
		return err
	}
	artifact := result.Artifact
	defer m.Jobs.Transport.Spool.Remove(artifact.Handle)
	opened, err := m.Jobs.Transport.Spool.Open(artifact.Handle)
	if err != nil {
		return access.Fail(http.StatusConflict, "media_content_unavailable", "The staged media could not be read.")
	}
	defer opened.File.Close()
	if err := m.audit(r, p, "media_job.content_download", record.ID); err != nil {
		return err
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(2 * time.Minute))
	w.Header().Set("Content-Type", artifact.ContentType)
	if artifact.ContentLength > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(artifact.ContentLength, 10))
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\""+contentFilename(record.ID, variant, artifact.ContentType)+"\"")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, opened.File); err != nil {
		m.log().Warn("media job content delivery failed", "job_id", record.ID)
	}
	return nil
}

func (m *Management) delete(r *http.Request, _ access.Principal) (access.Reply, error) {
	p, record, err := m.reauthorizeJob(r, access.Change)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, record.ETag); err != nil {
		return access.Reply{}, err
	}
	if err := m.jobsAvailable(); err != nil {
		return access.Reply{}, err
	}
	updated, err := m.Jobs.DeleteJob(r.Context(), record.ID)
	if err != nil {
		return access.Reply{}, mapJobError(err)
	}
	if updated.Lifecycle != LifecycleDeleted {
		// The provider delete was requested even though it is not confirmed.
		if err := m.audit(r, p, "media_job.delete_pending", record.ID); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{}, access.Fail(http.StatusConflict, "media_job_delete_pending", "The media job delete was initiated but the provider has not confirmed; reconciliation continues.")
	}
	if err := m.audit(r, p, "media_job.delete", record.ID); err != nil {
		return access.Reply{}, err
	}
	return access.Reply{Status: http.StatusNoContent}, nil
}

func contentFilename(jobID, variant, contentType string) string {
	ext := ".bin"
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])) {
	case "video/mp4":
		ext = ".mp4"
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	case "image/gif":
		ext = ".gif"
	}
	return "olp-media-" + jobID + "-" + variant + ext
}

func (m *Management) list(r *http.Request, p access.Principal) (access.Reply, error) {
	query := r.URL.Query()
	filters := Filters{AllProjects: p.AllProjects, AllowedProjects: p.ProjectIDs()}
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

func (m *Management) get(r *http.Request, p access.Principal) (access.Reply, error) {
	record, err := m.scopedJob(r, m.Pool, p, access.View)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(jobItem(record), record.ETag), nil
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
	if errors.Is(err, ErrContentUnavailable) {
		return access.Fail(http.StatusConflict, "media_content_unavailable", "The media job has no downloadable content.")
	}
	if jobErr, ok := errors.AsType[*JobError](err); ok {
		switch jobErr.Kind {
		case JobErrorNotFound:
			return access.Fail(http.StatusNotFound, "not_found", "The media job does not exist.")
		case JobErrorPrecondition:
			return access.Fail(http.StatusPreconditionFailed, "etag_mismatch", "The media job changed; refresh it and retry with the current ETag.")
		case JobErrorUpstreamIdentityConflict:
			return access.Fail(http.StatusConflict, "media_job_upstream_identity_conflict", "The upstream media job is already bound to different metadata.")
		case JobErrorBusy:
			return access.Fail(http.StatusConflict, "media_job_busy", "The media job is being reconciled by another worker; retry shortly.")
		case JobErrorInvalid:
			return access.Invalid("media_job", jobErr.Message)
		}
	}
	return err
}
