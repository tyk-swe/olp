package media

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s surface) videoList(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.EnterPrioritized(w, r, wire.Chat)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	x.UnaryFamily(wire.VideoList)
	ctx, complete, admissionError := s.kernel.AdmitKeyRequest(r.Context(), x, authority, kernel.MaxStreamDuration+jobs.LeaseSlack)
	if admissionError != nil {
		s.kernel.Refuse(x, w, admissionError)
		return
	}
	defer complete()
	r = r.WithContext(ctx)
	query, failure := media.ValidateVideoListQuery(r.URL.Query())
	if failure != nil {
		s.kernel.Refuse(x, w, mediaError(failure))
		return
	}
	routes := jobs.Everything
	if allowed := authority.RouteAllowlist(); allowed != nil {
		routes = jobs.Only(allowed)
	}
	listing := jobs.Query{
		Projects: jobs.Everything, Routes: routes, APIKeyID: &authority.ID,
		Operation: new(media.OpVideoCreate), Surface: new("openai"), Lifecycle: new(jobs.Active),
		Order: query.Order, Limit: query.Limit,
	}
	after, err := s.deps.Store.Position(r.Context(), cursorUUID(query.After), listing)
	if err != nil {
		s.kernel.Refuse(x, w, jobError(err))
		return
	}
	listing.After = after
	page, err := s.deps.Store.List(r.Context(), listing)
	if err != nil {
		s.kernel.Refuse(x, w, jobError(err))
		return
	}
	refreshed := make([]jobs.Record, 0, len(page.Items))
	var refreshError *fault.Error
	for _, record := range page.Items {
		updated, e := s.refreshListRecord(r.Context(), x, record)
		refreshed = append(refreshed, updated)
		if e != nil {
			refreshError = e
			break
		}
	}
	x.SummarizeAttemptVendors()
	if refreshError != nil {
		s.kernel.Refuse(x, w, refreshError)
		return
	}
	results := make([]media.VideoJobResult, 0, len(refreshed))
	for _, record := range refreshed {
		results = append(results, mediaJobResult(&record))
	}
	var firstID, lastID *string
	if len(results) > 0 {
		first := results[0].ID
		last := results[len(results)-1].ID
		firstID, lastID = &first, &last
	}
	body, failure := media.EncodeVideoListResponse(&media.VideoListResult{Jobs: results, FirstID: firstID, LastID: lastID, HasMore: page.More})
	if failure != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, failure.Code, failure.Message))
		return
	}
	x.MarkDispatched()
	delivered := s.deliverMediaJSON(w, x, http.StatusOK, false, body)
	s.kernel.Finish(x, &delivered.outcome, delivered.status)
}

func (s surface) admitOwnedVideo(w http.ResponseWriter, r *http.Request, family wire.Format, op media.Operation, allowVariant bool) (context.Context, *kernel.Call, *jobs.Record, func(), bool) {
	x, authority, done := s.kernel.EnterPrioritized(w, r, wire.Chat)
	if done {
		return nil, nil, nil, nil, false
	}
	release := func() { s.kernel.Gate.Release(r.Context()) }
	x.UnaryFamily(family)
	record, e := s.ownedJob(r.Context(), authority, r.PathValue("video_id"), op)
	if e != nil {
		release()
		s.kernel.Refuse(x, w, e)
		return nil, nil, nil, nil, false
	}
	if e = strictVideoResourceContext(x, record, allowVariant); e != nil {
		release()
		s.kernel.Refuse(x, w, e)
		return nil, nil, nil, nil, false
	}
	x.Route = videoJobRoute(record)
	ctx, complete, admissionError := s.kernel.AdmitKeyRequest(r.Context(), x, authority, kernel.MaxStreamDuration+jobs.LeaseSlack)
	if admissionError != nil {
		release()
		s.kernel.Refuse(x, w, admissionError)
		return nil, nil, nil, nil, false
	}
	return ctx, x, record, func() {
		complete()
		release()
	}, true
}

func (s surface) videoGet(w http.ResponseWriter, r *http.Request) {
	ctx, x, record, cleanup, ok := s.admitOwnedVideo(w, r, wire.VideoGet, media.OpVideoGet, false)
	if !ok {
		return
	}
	defer cleanup()
	upstreamID, ok := record.Upstream()
	if !ok {
		s.kernel.Refuse(x, w, upstreamIDUnavailable())
		return
	}
	result, transportFailure, fact := s.videoJobCall(ctx, x, record, media.VideoGet{ID: upstreamID})
	x.NoteDispatchFailure(transportFailure)
	x.NoteAttempt(fact)
	if transportFailure != nil {
		s.failMediaOutcome(x, w, transportFailure)
		return
	}
	update, ok := jobs.Observe(mediaVideo(result), s.kernel.Now())
	if !ok {
		s.failMediaOutcome(x, w, kernel.AttemptFailureFor(kernel.ClassProtocol))
		return
	}
	updated, err := s.deps.Store.Refresh(ctx, record.ID, jobs.Unfenced, update)
	if err != nil {
		s.failMediaOutcome(x, w, kernel.AttemptFailureFor(kernel.ClassConnect))
		return
	}
	var body []byte
	var renderFailure *media.Error
	if record.StrictContract {
		body, renderFailure = media.EncodeStrictVideoObject(result.Source, upstreamID, updated.ID, updated.RouteSlug)
	} else {
		body, renderFailure = media.EncodeVideoObject(mediaVideo(result), updated.ID, updated.RouteSlug)
	}
	if renderFailure != nil {
		s.failMediaOutcome(x, w, kernel.AttemptFailureFor(kernel.ClassProtocol))
		return
	}
	delivered := s.deliverMediaJSON(w, x, http.StatusOK, true, body)
	s.kernel.Finish(x, &delivered.outcome, delivered.status)
}

func (s surface) videoContent(w http.ResponseWriter, r *http.Request) {
	ctx, x, record, cleanup, ok := s.admitOwnedVideo(w, r, wire.VideoContent, media.OpVideoContent, true)
	if !ok {
		return
	}
	defer cleanup()
	variant, failure := media.ValidateVideoContentQuery(r.URL.Query())
	if failure != nil {
		s.kernel.Refuse(x, w, mediaError(failure))
		return
	}
	upstreamID, ok := record.Upstream()
	if !ok {
		s.kernel.Refuse(x, w, upstreamIDUnavailable())
		return
	}
	result, transportFailure, fact := s.videoJobCall(ctx, x, record, media.VideoContent{ID: upstreamID, Variant: variant})
	x.NoteDispatchFailure(transportFailure)
	x.NoteAttempt(fact)
	if transportFailure != nil {
		s.failMediaOutcome(x, w, transportFailure)
		return
	}
	artifact := mediaArtifact(result.Payload)
	if artifact == nil {
		s.failMediaOutcome(x, w, kernel.AttemptFailureFor(kernel.ClassProtocol))
		return
	}
	delivered := s.streamMediaArtifact(w, x, http.StatusOK, true, artifact)
	s.kernel.Finish(x, &delivered.outcome, delivered.status)
}

func (s surface) videoDelete(w http.ResponseWriter, r *http.Request) {
	ctx, x, record, cleanup, ok := s.admitOwnedVideo(w, r, wire.VideoDelete, media.OpVideoDelete, false)
	if !ok {
		return
	}
	defer cleanup()
	if record.Lifecycle == jobs.Deleted {
		s.repeatVideoDeletion(w, x, record)
		return
	}
	upstreamID, ok := record.Upstream()
	if !ok {
		s.kernel.Refuse(x, w, upstreamIDUnavailable())
		return
	}
	result, transportFailure, fact := s.videoJobCall(ctx, x, record, media.VideoDelete{ID: upstreamID})
	x.NoteDispatchFailure(transportFailure)
	x.NoteAttempt(fact)
	if undeleted := videoUndeleted(result, transportFailure); undeleted != nil {
		s.failMediaOutcome(x, w, undeleted)
		return
	}
	finalized, err := s.deps.Store.Finalize(ctx, record.ID, jobs.Unfenced)
	if err != nil {
		s.kernel.Refuse(x, w, jobError(err))
		return
	}
	if finalized.Lifecycle != jobs.Deleted {
		s.deps.Jobs.RecordGap()
		s.failMediaOutcome(x, w, kernel.AttemptFailureFor(kernel.ClassConnect))
		return
	}
	if record.StrictContract && result == nil {
		s.failMediaOutcome(x, w, transportFailure)
		return
	}
	body, renderFailure := videoDeletion(record, upstreamID, result)
	if renderFailure != nil {
		s.failMediaOutcome(x, w, kernel.AttemptFailureFor(kernel.ClassProtocol))
		return
	}
	delivered := s.deliverMediaJSON(w, x, http.StatusOK, result != nil, body)
	s.kernel.Finish(x, &delivered.outcome, delivered.status)
}

func (s surface) repeatVideoDeletion(w http.ResponseWriter, x *kernel.Call, record *jobs.Record) {
	if record.StrictContract {
		s.kernel.Refuse(x, w, fault.NotFoundError("video_not_found", "The video job does not exist."))
		return
	}
	body, failure := media.EncodeVideoDeleteResponse(&media.VideoDeleteResult{ID: record.ID, Deleted: true}, record.ID)
	if failure != nil {
		s.kernel.Refuse(x, w, mediaError(failure))
		return
	}
	x.MarkDispatched()
	delivered := s.deliverMediaJSON(w, x, http.StatusOK, false, body)
	s.kernel.Finish(x, &delivered.outcome, delivered.status)
}

func upstreamIDUnavailable() *fault.Error {
	return fault.ServerError(http.StatusServiceUnavailable, "media_job_upstream_id_unavailable", "The video job's upstream identity is unavailable.")
}

func jobError(err error) *fault.Error {
	failure, ok := errors.AsType[*jobs.Error](err)
	if !ok {
		return mediaError(media.Fail(http.StatusServiceUnavailable, "media_job_unavailable", "The media job store is unavailable."))
	}
	switch failure.Kind {
	case jobs.KindNotFound:
		return mediaError(media.Fail(http.StatusNotFound, "media_job_not_found", "The media job does not exist."))
	case jobs.KindChanged:
		return mediaError(media.Fail(http.StatusConflict, "media_job_changed", "The media job changed; refresh and retry."))
	case jobs.KindConflict:
		return mediaError(media.Fail(http.StatusConflict, "media_job_upstream_conflict", "The upstream media job identity conflicts with stored metadata."))
	case jobs.KindInvalid:
		return mediaError(media.Fail(http.StatusBadRequest, "invalid_request", failure.Message))
	}
	return mediaError(media.Fail(http.StatusServiceUnavailable, "media_job_unavailable", "The media job store is unavailable."))
}

func (s surface) ownedJob(ctx context.Context, authority keyauthority.Authority, videoID string, op media.Operation) (*jobs.Record, *fault.Error) {
	id, err := uuid.Parse(videoID)
	if err != nil {
		return nil, fault.NotFoundError("video_not_found", "The video job does not exist.")
	}
	record, err := s.deps.Store.Get(ctx, id.String())
	if err != nil {
		return nil, jobError(err)
	}
	if record.APIKeyID != authority.ID {
		return nil, fault.NotFoundError("video_not_found", "The video job does not exist.")
	}
	if record.Lifecycle == jobs.Deleted && op != media.OpVideoDelete {
		return nil, fault.NotFoundError("video_not_found", "The video job does not exist.")
	}
	switch record.Lifecycle {
	case jobs.Active, jobs.DeletePending, jobs.Deleted:
	default:
		return nil, fault.ServerError(http.StatusServiceUnavailable, "media_job_reconciliation_pending", "The video job is being reconciled; retry shortly.")
	}
	projectID, current, e := s.jobRoute(ctx, record.RouteSlug)
	if e != nil {
		return nil, e
	}
	if !authority.Allows("inference", record.RouteSlug, projectID, s.kernel.Now()) {
		return nil, fault.NotFoundError("video_not_found", "The video job does not exist.")
	}
	use := kernel.RetainedRetrieval
	if op == media.OpVideoDelete {
		use = kernel.RetainedHousekeeping
	}
	if e := kernel.RetainedContract(record.StrictContract, current, use); e != nil {
		return nil, e
	}
	return &record, nil
}

func (s surface) jobRoute(ctx context.Context, slug string) (*string, routing.RouteFidelity, *fault.Error) {
	projectID, current, err := s.deps.Store.RouteAuthority(ctx, slug)
	if err != nil {
		return nil, routing.RouteFidelity{}, fault.ServerError(http.StatusServiceUnavailable, "route_unavailable", "The video job's route could not be read.")
	}
	return projectID, current, nil
}

func strictVideoResourceContext(x *kernel.Call, record *jobs.Record, allowVariant bool) *fault.Error {
	if !record.StrictContract {
		return nil
	}
	if e := strictMediaHeaders(x); e != nil {
		return e
	}
	query, invalid := x.SemanticQuery()
	if invalid || (!allowVariant && len(query) != 0) {
		return fault.InvalidRequest("target_capability", "This strict video resource does not qualify caller query settings.", nil)
	}
	return nil
}

func videoJobRoute(record *jobs.Record) *routing.Route {
	route := &routing.Route{Slug: record.RouteSlug, Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed}}
	if record.StrictContract {
		route.Fidelity.Mode = routing.FidelityStrict
	}
	return route
}

func videoUndeleted(result *transport.Result, failure *kernel.AttemptFailure) *kernel.AttemptFailure {
	if failure != nil {
		if failure.Status != http.StatusNotFound {
			return failure
		}
		return nil
	}
	receipt := mediaDeleted(result)
	if receipt == nil {
		return kernel.AttemptFailureFor(kernel.ClassProtocol)
	}
	if !receipt.Deleted {
		return kernel.NewAttemptFailure(kernel.AttemptFailureSpec{Class: kernel.ClassUpstreamClient, Status: http.StatusBadGateway})
	}
	return nil
}

func videoDeletion(record *jobs.Record, upstreamID string, result *transport.Result) ([]byte, *media.Error) {
	if record.StrictContract {
		return media.EncodeStrictVideoObject(result.Source, upstreamID, record.ID, "")
	}
	receipt := mediaDeleted(result)
	if receipt == nil {
		return media.EncodeVideoDeleteResponse(&media.VideoDeleteResult{Deleted: true}, record.ID)
	}
	return media.EncodeVideoDeleteResponse(&media.VideoDeleteResult{Deleted: true, Extra: receipt.Extra}, record.ID)
}

func mediaJobResult(record *jobs.Record) media.VideoJobResult {
	status := "queued"
	switch record.State {
	case jobs.Running:
		status = "in_progress"
	case jobs.Succeeded:
		status = "completed"
	case jobs.Failed:
		status = "failed"
	case jobs.Cancelled:
		status = "cancelled"
	}
	result := media.VideoJobResult{
		ID:       record.ID,
		Route:    record.RouteSlug,
		Status:   status,
		Progress: record.ProgressPercent,
	}
	if created := record.CreatedAt.Unix(); created > 0 {
		result.CreatedAt = &created
	}
	if record.CompletedAt != nil {
		completed := record.CompletedAt.Unix()
		result.CompletedAt = &completed
	}
	if record.ExpiresAt != nil {
		expires := record.ExpiresAt.Unix()
		result.ExpiresAt = &expires
	}
	if record.ErrorClass != nil {
		result.ErrorMessage = record.ErrorClass
	}
	return result
}

func cursorUUID(value *uuid.UUID) *string {
	if value == nil {
		return nil
	}
	s := value.String()
	return &s
}
