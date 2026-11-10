package jobs

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/staging"
)

type API struct {
	Access *access.Server
	Jobs   *Service
	Spool  *staging.Spool
	Log    *slog.Logger
}

const operatorJobTimeout = 5 * time.Minute

func (m *API) Register(mux *http.ServeMux) {
	m.Access.Route(mux, "GET /api/v1/media-jobs", m.list)
	m.Access.Route(mux, "GET /api/v1/media-jobs/{job_id}", m.get)
	m.Access.Route(mux, "POST /api/v1/media-jobs/{job_id}/refresh", m.refresh, access.MaxBody(1024), access.Deadline(operatorJobTimeout))
	m.Access.Stream(mux, "GET /api/v1/media-jobs/{job_id}/content", m.content, access.MaxBody(1024), access.Deadline(operatorJobTimeout))
	m.Access.Route(mux, "DELETE /api/v1/media-jobs/{job_id}", m.delete, access.MaxBody(1024), access.Deadline(operatorJobTimeout))
}

var errJobNotFound = access.Fail(http.StatusNotFound, "not_found", "The media job does not exist.")

func scoped(r *http.Request, store *Store, p access.Principal, need access.Need) (Record, error) {
	id := r.PathValue("job_id")
	if _, err := uuid.Parse(id); err != nil {
		return Record{}, errJobNotFound
	}
	record, err := store.Get(r.Context(), id)
	if err != nil {
		return Record{}, problem(err)
	}
	if err := p.Project(record.ProjectID, need); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (m *API) reauthorize(r *http.Request) (access.Principal, Record, error) {
	tx, p, err := m.Access.BeginMutation(r)
	if err != nil {
		return access.Principal{}, Record{}, err
	}
	defer tx.Rollback(r.Context())
	record, err := scoped(r, NewStore(tx), p, access.Change)
	return p, record, err
}

func (m *API) audit(r *http.Request, p access.Principal, action, id string) error {
	tx, err := m.Access.Pool.Begin(r.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(r.Context())
	if err := access.Audit(r.Context(), tx, r, p.Actor(), action, "media_job", id, "success"); err != nil {
		return err
	}
	return tx.Commit(r.Context())
}

func (m *API) get(r *http.Request, p access.Principal) (access.Reply, error) {
	record, err := scoped(r, m.Jobs.store, p, access.View)
	if err != nil {
		return access.Reply{}, err
	}
	return detail(record)
}

func detail(record Record) (access.Reply, error) {
	item, err := jobItem(record)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(item, record.ETag), nil
}

func (m *API) refresh(r *http.Request, _ access.Principal) (access.Reply, error) {
	p, record, err := m.reauthorize(r)
	if err != nil {
		return access.Reply{}, err
	}
	updated, err := m.Jobs.Refresh(r.Context(), record.ID)
	if err != nil {
		return access.Reply{}, problem(err)
	}
	if err := m.audit(r, p, "media_job.refresh", updated.ID); err != nil {
		return access.Reply{}, err
	}
	return detail(updated)
}

func (m *API) content(w http.ResponseWriter, r *http.Request, p access.Principal) error {
	record, err := scoped(r, m.Jobs.store, p, access.Change)
	if err != nil {
		return err
	}
	variant, failure := media.ValidateVideoContentQuery(r.URL.Query())
	if failure != nil {
		return access.Fail(failure.Status, failure.Code, failure.Message)
	}
	artifact, err := m.Jobs.Content(r.Context(), record, variant)
	if errors.Is(err, ErrNoContent) {
		return access.Fail(http.StatusConflict, "media_content_unavailable", "The media job has no downloadable "+variant+" content.")
	}
	if err != nil {
		return access.Fail(http.StatusServiceUnavailable, "media_content_unavailable", "The provider could not return the media job content; retry shortly.")
	}
	defer m.Spool.Remove(artifact.Handle)
	opened, err := m.Spool.Open(artifact.Handle)
	if err != nil {
		return access.Fail(http.StatusConflict, "media_content_unavailable", "The staged media could not be read.")
	}
	defer opened.File.Close()
	if err := m.audit(r, p, "media_job.content_download", record.ID); err != nil {
		return err
	}
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		return access.Fail(http.StatusInternalServerError, "internal_error", "The media response could not be bounded.")
	}
	w.Header().Set("Content-Type", artifact.ContentType)
	if artifact.ContentLength > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(artifact.ContentLength, 10))
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\""+contentFilename(record.ID, variant, artifact.ContentType)+"\"")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, opened.File); err != nil {
		m.Log.Warn("media job content delivery failed", "job_id", record.ID)
	}
	return nil
}

func (m *API) delete(r *http.Request, _ access.Principal) (access.Reply, error) {
	p, record, err := m.reauthorize(r)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, record.ETag); err != nil {
		return access.Reply{}, err
	}
	updated, err := m.Jobs.Delete(r.Context(), record.ID)
	if err != nil {
		return access.Reply{}, problem(err)
	}
	if updated.Lifecycle != Deleted {
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
	extensions := map[string]string{"video/mp4": ".mp4", "image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif"}
	mediaType, _, _ := strings.Cut(contentType, ";")
	extension, ok := extensions[strings.ToLower(strings.TrimSpace(mediaType))]
	if !ok {
		extension = ".bin"
	}
	return "olp-media-" + jobID + "-" + variant + extension
}

func optionalUUID(raw, field, message string) (*string, error) {
	if raw == "" {
		return nil, nil
	}
	if _, err := uuid.Parse(raw); err != nil {
		return nil, access.Invalid(field, message)
	}
	return &raw, nil
}

func optionalTime(raw, field string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, access.Invalid(field, "Use an RFC 3339 timestamp.")
	}
	return &at, nil
}

func optionalEnum[T any](raw, field, message string, parse func(string) (T, bool)) (*T, error) {
	if raw == "" {
		return nil, nil
	}
	value, ok := parse(raw)
	if !ok {
		return nil, access.Invalid(field, message)
	}
	return &value, nil
}

func pageLimit(raw string) (int, error) {
	if raw == "" {
		return 50, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 200 {
		return 0, access.Invalid("limit", "Page size must be between 1 and 200.")
	}
	return limit, nil
}

func encodeCursor(record Record) string {
	return record.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + record.ID
}

func decodeCursor(raw string) (*Position, error) {
	if raw == "" {
		return nil, nil
	}
	at, id, ok := strings.Cut(raw, "|")
	created, err := time.Parse(time.RFC3339Nano, at)
	if _, idErr := uuid.Parse(id); !ok || err != nil || idErr != nil {
		return nil, access.Fail(http.StatusBadRequest, "invalid_cursor", "The page cursor is malformed; restart the listing.")
	}
	return &Position{CreatedAt: created, ID: id}, nil
}

func (m *API) list(r *http.Request, p access.Principal) (access.Reply, error) {
	query := r.URL.Query()
	apiKey, keyErr := optionalUUID(query.Get("api_key_id"), "api_key_id", "Use an API key identifier.")
	provider, providerErr := optionalUUID(query.Get("provider_id"), "provider_id", "Use a provider identifier.")
	state, stateErr := optionalEnum(query.Get("state"), "state", "State must be queued, running, succeeded, failed, or cancelled.", ParseState)
	lifecycle, lifecycleErr := optionalEnum(query.Get("lifecycle"), "lifecycle", "Unknown media-job reconciliation lifecycle.", ParseLifecycle)
	after, afterErr := optionalTime(query.Get("created_after"), "created_after")
	before, beforeErr := optionalTime(query.Get("created_before"), "created_before")
	limit, limitErr := pageLimit(query.Get("limit"))
	cursor, cursorErr := decodeCursor(query.Get("cursor"))
	if err := first(keyErr, providerErr, stateErr, lifecycleErr, afterErr, beforeErr, limitErr, cursorErr); err != nil {
		return access.Reply{}, err
	}
	if after != nil && before != nil && !after.Before(*before) {
		return access.Reply{}, access.Invalid("created_before", "created_before must be after created_after.")
	}
	projects := Everything
	if !p.AllProjects {
		projects = Only(p.ProjectIDs())
	}
	routes := Everything
	if raw := query.Get("route"); raw != "" {
		routes = Only([]string{raw})
	}
	page, err := m.Jobs.store.List(r.Context(), Query{
		Projects: projects, Routes: routes, APIKeyID: apiKey, ProviderID: provider,
		State: state, Lifecycle: lifecycle, CreatedAfter: after, CreatedBefore: before,
		After: cursor, Order: media.OrderDescending, Limit: limit,
	})
	if err != nil {
		return access.Reply{}, problem(err)
	}
	items := make([]contract.MediaJobItem, len(page.Items))
	for i, record := range page.Items {
		item, err := jobItem(record)
		if err != nil {
			return access.Reply{}, err
		}
		items[i] = item
	}
	response := contract.MediaJobListResponse{Items: items}
	if page.More {
		response.NextCursor = nullable.NewNullableWithValue(encodeCursor(page.Items[len(page.Items)-1]))
	}
	return access.OK(response), nil
}

func first(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func nullableOf[T any](value *T) nullable.Nullable[T] {
	if value == nil {
		return nil
	}
	return nullable.NewNullableWithValue(*value)
}

func jobItem(record Record) (contract.MediaJobItem, error) {
	id, idErr := uuid.Parse(record.ID)
	key, keyErr := uuid.Parse(record.APIKeyID)
	provider, providerErr := uuid.Parse(record.ProviderID)
	if err := errors.Join(idErr, keyErr, providerErr); err != nil {
		return contract.MediaJobItem{}, err
	}
	return contract.MediaJobItem{
		Id:                  id,
		ApiKeyId:            key,
		ProviderId:          provider,
		ProviderName:        record.ProviderName,
		ProviderModel:       record.UpstreamModel,
		Route:               record.RouteSlug,
		Operation:           string(record.Operation),
		Surface:             record.Surface,
		State:               string(record.State),
		Lifecycle:           string(record.Lifecycle),
		ContentAvailable:    record.ContentAvailable(),
		Etag:                record.ETag,
		CreatedAt:           record.CreatedAt,
		UpdatedAt:           record.UpdatedAt,
		UpstreamJobId:       nullableOf(record.UpstreamJobID),
		ProgressPercent:     nullableOf(record.ProgressPercent),
		ExpiresAt:           nullableOf(record.ExpiresAt),
		ErrorClass:          nullableOf(record.ErrorClass),
		CompletedAt:         nullableOf(record.CompletedAt),
		LastPolledAt:        nullableOf(record.LastPolledAt),
		ReconciliationError: nullableOf(record.ReconciliationError),
		DeletedAt:           nullableOf(record.DeletedAt),
	}, nil
}

func problem(err error) error {
	failure, ok := errors.AsType[*Error](err)
	if !ok {
		return err
	}
	switch failure.Kind {
	case KindNotFound:
		return errJobNotFound
	case KindChanged:
		return access.Fail(http.StatusPreconditionFailed, "etag_mismatch", "The media job changed; refresh it and retry with the current ETag.")
	case KindConflict:
		return access.Fail(http.StatusConflict, "media_job_upstream_identity_conflict", "The upstream media job is already bound to different metadata.")
	case KindBusy:
		return access.Fail(http.StatusConflict, "media_job_busy", "The media job is being reconciled by another worker; retry shortly.")
	case KindInvalid:
		return access.Invalid("media_job", failure.Message)
	}
	return err
}
