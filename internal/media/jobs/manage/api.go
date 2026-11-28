package manage

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/platform/staging"
)

type API struct {
	Access *access.Server
	Jobs   *jobs.Service
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

var (
	errJobNotFound   = problem.Fail(http.StatusNotFound, "not_found", "The media job does not exist.")
	errDeletePending = problem.Fail(http.StatusConflict, "media_job_delete_pending", "The media job delete was initiated but the provider has not confirmed; reconciliation continues.")
)

func (m *API) logger() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.New(slog.DiscardHandler)
}

func scoped(r *http.Request, store *jobs.Store, p authz.Principal, need authz.Need) (jobs.Record, error) {
	id, err := uuid.Parse(r.PathValue("job_id"))
	if err != nil {
		return jobs.Record{}, errJobNotFound
	}
	record, err := store.Get(r.Context(), id.String())
	if err != nil {
		return jobs.Record{}, jobProblem(err)
	}
	if err := p.Project(record.ProjectID, need); err != nil {
		return jobs.Record{}, err
	}
	return record, nil
}

func (m *API) get(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	record, err := scoped(r, m.Jobs.Store(), p, authz.View)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return detail(record)
}

func detail(record jobs.Record) (httpkit.Reply, error) {
	item, err := jobItem(record)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(item, record.ETag), nil
}

func (m *API) refresh(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	if _, err := uuid.Parse(r.PathValue("job_id")); err != nil {
		return httpkit.Reply{}, errJobNotFound
	}
	record, err := access.Run(m.Access, r, func(tx pgx.Tx, p authz.Principal) (jobs.Record, access.Event, error) {
		rec, err := scoped(r, jobs.NewStore(tx), p, authz.Change)
		return rec, access.Event{}, err
	})
	if err != nil {
		return httpkit.Reply{}, err
	}
	updated, err := m.Jobs.Refresh(r.Context(), record.ID)
	if err != nil {
		return httpkit.Reply{}, jobProblem(err)
	}
	return access.Transact(m.Access, r, access.Unlocked, func(_ pgx.Tx, _ authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		reply, err := detail(updated)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return reply, auditEvent("media_job.refresh", updated), nil
	})
}

func (m *API) content(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	record, err := scoped(r, m.Jobs.Store(), p, authz.Change)
	if err != nil {
		return err
	}
	variant, failure := media.ValidateVideoContentQuery(r.URL.Query())
	if failure != nil {
		return problem.Fail(failure.Status, failure.Code, failure.Message)
	}
	artifact, err := m.Jobs.Content(r.Context(), record, variant)
	if errors.Is(err, jobs.ErrNoContent) {
		return problem.Fail(http.StatusConflict, "media_content_unavailable", "The media job has no downloadable "+variant+" content.")
	}
	if err != nil {
		return problem.Fail(http.StatusServiceUnavailable, "media_content_unavailable", "The provider could not return the media job content; retry shortly.")
	}
	defer m.Spool.Remove(artifact.Handle)
	opened, err := m.Spool.Open(artifact.Handle)
	if err != nil {
		return problem.Fail(http.StatusConflict, "media_content_unavailable", "The staged media could not be read.")
	}
	defer opened.File.Close()
	if _, err := access.Run(m.Access, r, func(_ pgx.Tx, _ authz.Principal) (struct{}, access.Event, error) {
		return struct{}{}, auditEvent("media_job.content_download", record), nil
	}); err != nil {
		return err
	}
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		return problem.Fail(http.StatusInternalServerError, "internal_error", "The media response could not be bounded.")
	}
	w.Header().Set("Content-Type", artifact.ContentType)
	if artifact.ContentLength > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(artifact.ContentLength, 10))
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\""+contentFilename(record.ID, variant, artifact.ContentType)+"\"")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, opened.File); err != nil {
		m.logger().Warn("media job content delivery failed", "job_id", record.ID)
	}
	return nil
}

type deleteClaim struct {
	claimed        jobs.Claimed
	needsReconcile bool
}

func claimDeleteTx(r *http.Request, store *jobs.Store, p authz.Principal, now time.Time) (deleteClaim, access.Event, error) {
	record, err := scoped(r, store, p, authz.Change)
	if err != nil {
		return deleteClaim{}, access.Event{}, err
	}
	if err := httpkit.Match(r, record.ETag); err != nil {
		return deleteClaim{}, access.Event{}, err
	}
	claimed, needsReconcile, err := store.ClaimForDelete(r.Context(), record.ID, record.ETag, now)
	if err != nil {
		return deleteClaim{}, access.Event{}, jobProblem(err)
	}
	if !needsReconcile {
		return deleteClaim{}, auditEvent("media_job.delete", record), nil
	}
	return deleteClaim{claimed: claimed, needsReconcile: true}, access.Event{}, nil
}

func (m *API) delete(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	if _, err := uuid.Parse(r.PathValue("job_id")); err != nil {
		return httpkit.Reply{}, errJobNotFound
	}
	plan, err := access.Run(m.Access, r, func(tx pgx.Tx, p authz.Principal) (deleteClaim, access.Event, error) {
		return claimDeleteTx(r, jobs.NewStore(tx), p, m.Jobs.Now())
	})
	if err != nil {
		return httpkit.Reply{}, err
	}
	if !plan.needsReconcile {
		return httpkit.Reply{Status: http.StatusNoContent}, nil
	}
	updated, err := m.Jobs.ReconcileClaimed(r.Context(), plan.claimed)
	if err != nil {
		return httpkit.Reply{}, jobProblem(err)
	}
	if updated.Lifecycle != jobs.Deleted {
		if _, err := access.Run(m.Access, r, func(_ pgx.Tx, _ authz.Principal) (struct{}, access.Event, error) {
			return struct{}{}, auditEvent("media_job.delete_pending", updated), nil
		}); err != nil {
			return httpkit.Reply{}, err
		}
		return httpkit.Reply{}, errDeletePending
	}
	return access.Transact(m.Access, r, access.Unlocked, func(_ pgx.Tx, _ authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		return httpkit.Reply{Status: http.StatusNoContent}, auditEvent("media_job.delete", updated), nil
	})
}

func auditEvent(action string, record jobs.Record) access.Event {
	return access.Event{
		Action:   action,
		Resource: "media_job",
		ID:       record.ID,
		Project:  record.ProjectID,
	}
}

func contentExtension(contentType string) string {
	mediaType, _, _ := strings.Cut(contentType, ";")
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "video/mp4":
		return ".mp4"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".bin"
	}
}

func contentFilename(jobID, variant, contentType string) string {
	return "olp-media-" + jobID + "-" + variant + contentExtension(contentType)
}

func optionalUUID(raw, field, message string) (*string, error) {
	if raw == "" {
		return nil, nil
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return nil, problem.Invalid(field, message)
	}
	canonical := parsed.String()
	return &canonical, nil
}

func optionalTime(raw, field string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, problem.Invalid(field, "Use an RFC 3339 timestamp.")
	}
	return &at, nil
}

func optionalEnum[T any](raw, field, message string, parse func(string) (T, bool)) (*T, error) {
	if raw == "" {
		return nil, nil
	}
	value, ok := parse(raw)
	if !ok {
		return nil, problem.Invalid(field, message)
	}
	return &value, nil
}

func pageLimit(raw string) (int, error) {
	if raw == "" {
		return 50, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 200 {
		return 0, problem.Invalid("limit", "Page size must be between 1 and 200.")
	}
	return limit, nil
}

func encodeCursor(record jobs.Record) string {
	return record.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + record.ID
}

func decodeCursor(raw string) (*jobs.Position, error) {
	if raw == "" {
		return nil, nil
	}
	at, id, ok := strings.Cut(raw, "|")
	created, err := time.Parse(time.RFC3339Nano, at)
	parsed, idErr := uuid.Parse(id)
	if !ok || err != nil || idErr != nil {
		return nil, problem.Fail(http.StatusBadRequest, "invalid_cursor", "The page cursor is malformed; restart the listing.")
	}
	return &jobs.Position{CreatedAt: created, ID: parsed.String()}, nil
}

func parseListQuery(r *http.Request, p authz.Principal) (jobs.Query, error) {
	query := r.URL.Query()
	apiKey, keyErr := optionalUUID(query.Get("api_key_id"), "api_key_id", "Use an API key identifier.")
	provider, providerErr := optionalUUID(query.Get("provider_id"), "provider_id", "Use a provider identifier.")
	state, stateErr := optionalEnum(query.Get("state"), "state", "State must be queued, running, succeeded, failed, or cancelled.", jobs.ParseState)
	lifecycle, lifecycleErr := optionalEnum(query.Get("lifecycle"), "lifecycle", "Unknown media-job reconciliation lifecycle.", jobs.ParseLifecycle)
	after, afterErr := optionalTime(query.Get("created_after"), "created_after")
	before, beforeErr := optionalTime(query.Get("created_before"), "created_before")
	limit, limitErr := pageLimit(query.Get("limit"))
	cursor, cursorErr := decodeCursor(query.Get("cursor"))
	if err := first(keyErr, providerErr, stateErr, lifecycleErr, afterErr, beforeErr, limitErr, cursorErr); err != nil {
		return jobs.Query{}, err
	}
	if after != nil && before != nil && !after.Before(*before) {
		return jobs.Query{}, problem.Invalid("created_before", "created_before must be after created_after.")
	}
	projects := jobs.Everything
	if !p.AllProjects {
		projects = jobs.Only(p.ProjectIDs())
	}
	routes := jobs.Everything
	if raw := query.Get("route"); raw != "" {
		routes = jobs.Only([]string{raw})
	}
	return jobs.Query{
		Projects: projects, Routes: routes, APIKeyID: apiKey, ProviderID: provider,
		State: state, Lifecycle: lifecycle, CreatedAfter: after, CreatedBefore: before,
		After: cursor, Order: media.OrderDescending, Limit: limit,
	}, nil
}

func (m *API) list(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	listing, err := parseListQuery(r, p)
	if err != nil {
		return httpkit.Reply{}, err
	}
	page, err := m.Jobs.Store().List(r.Context(), listing)
	if err != nil {
		return httpkit.Reply{}, jobProblem(err)
	}
	items := make([]contract.MediaJobItem, len(page.Items))
	for i, record := range page.Items {
		item, err := jobItem(record)
		if err != nil {
			return httpkit.Reply{}, err
		}
		items[i] = item
	}
	response := contract.MediaJobListResponse{Items: items}
	if page.More {
		response.NextCursor = nullable.NewNullableWithValue(encodeCursor(page.Items[len(page.Items)-1]))
	}
	return httpkit.OK(response), nil
}

func first(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func valueOrOmitted[T any](value *T) nullable.Nullable[T] {
	if value == nil {
		return nil
	}
	return nullable.NewNullableWithValue(*value)
}

func jobItem(record jobs.Record) (contract.MediaJobItem, error) {
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
		UpstreamJobId:       valueOrOmitted(record.UpstreamJobID),
		ProgressPercent:     valueOrOmitted(record.ProgressPercent),
		ExpiresAt:           valueOrOmitted(record.ExpiresAt),
		ErrorClass:          valueOrOmitted(record.ErrorClass),
		CompletedAt:         valueOrOmitted(record.CompletedAt),
		LastPolledAt:        valueOrOmitted(record.LastPolledAt),
		ReconciliationError: valueOrOmitted(record.ReconciliationError),
		DeletedAt:           valueOrOmitted(record.DeletedAt),
	}, nil
}

func jobProblem(err error) error {
	failure, ok := errors.AsType[*jobs.Error](err)
	if !ok {
		return err
	}
	switch failure.Kind {
	case jobs.KindNotFound:
		return errJobNotFound
	case jobs.KindChanged:
		return problem.Fail(http.StatusPreconditionFailed, "etag_mismatch", "The media job changed; refresh it and retry with the current ETag.")
	case jobs.KindConflict:
		return problem.Fail(http.StatusConflict, "media_job_upstream_identity_conflict", "The upstream media job is already bound to different metadata.")
	case jobs.KindBusy:
		return problem.Fail(http.StatusConflict, "media_job_busy", "The media job is being reconciled by another worker; retry shortly.")
	case jobs.KindInvalid:
		return problem.Invalid("media_job", failure.Message)
	case jobs.KindCallerCredential:
		return problem.Fail(http.StatusConflict, "media_job_caller_credential", "The media job runs on the caller's credential, so only the caller's own requests can refresh or delete it.")
	}
	return err
}
