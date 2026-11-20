package media

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/jobs"
)

func (s surface) videoList(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.BeginMedia(w, r)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	x.UnaryFamily(wire.VideoList)
	ctx, complete, admissionError := s.kernel.AdmitVideoRequest(r.Context(), x, authority)
	if admissionError != nil {
		s.kernel.Refuse(x, w, admissionError)
		return
	}
	defer complete()
	r = r.WithContext(ctx)
	query, failure := media.ValidateVideoListQuery(r.URL.Query())
	if failure != nil {
		s.kernel.Refuse(x, w, kernel.MediaError(failure))
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
	after, err := s.kernel.Deps.Media.Store.Position(r.Context(), cursorUUID(query.After), listing)
	if err != nil {
		s.kernel.Refuse(x, w, kernel.JobError(err))
		return
	}
	listing.After = after
	page, err := s.kernel.Deps.Media.Store.List(r.Context(), listing)
	if err != nil {
		s.kernel.Refuse(x, w, kernel.JobError(err))
		return
	}
	refreshed := make([]jobs.Record, 0, len(page.Items))
	var refreshError *fault.Error
	for _, record := range page.Items {
		updated, e := s.kernel.RefreshListRecord(r.Context(), x, record)
		refreshed = append(refreshed, updated)
		if e != nil {
			refreshError = e
			break
		}
	}
	x.NotePolls()
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
	list := &media.VideoListResult{Jobs: results, FirstID: firstID, LastID: lastID, HasMore: page.More}
	body, failure := media.EncodeVideoListResponse(list)
	if failure != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, failure.Code, failure.Message))
		return
	}
	x.MarkDispatched()
	out := kernel.MediaStatusOutcome(http.StatusOK)
	s.kernel.DeliverMediaJSON(w, x, out, body)
	s.kernel.FinishMedia(x, out, out.Status())
}

func (s surface) videoGet(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.BeginMedia(w, r)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	x.UnaryFamily(wire.VideoGet)
	record, e := s.kernel.OwnedJob(r.Context(), authority, r.PathValue("video_id"), media.OpVideoGet)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if e = kernel.StrictVideoResourceContext(x, record, false); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	x.Route = kernel.VideoJobRoute(record)
	ctx, complete, admissionError := s.kernel.AdmitVideoRequest(r.Context(), x, authority)
	if admissionError != nil {
		s.kernel.Refuse(x, w, admissionError)
		return
	}
	defer complete()
	r = r.WithContext(ctx)
	upstream, ok := record.Upstream()
	if !ok {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "media_job_upstream_id_unavailable", "The video job's upstream identity is unavailable."))
		return
	}
	result, transportFailure, fact := s.kernel.VideoJobCall(r.Context(), x, record, media.VideoGet{ID: upstream})
	x.NoteDispatchFailure(transportFailure)
	x.NoteJobAttempt(fact)
	if transportFailure != nil {
		s.kernel.FailMediaOutcome(x, w, transportFailure)
		return
	}
	update, ok := jobs.Observe(kernel.MediaVideo(result), s.kernel.Now())
	if !ok {
		s.kernel.FailMediaOutcome(x, w, kernel.AttemptFailureFor(kernel.ClassProtocol))
		return
	}
	updated, err := s.kernel.Deps.Media.Store.Refresh(r.Context(), record.ID, jobs.Unfenced, update)
	if err != nil {
		s.kernel.FailMediaOutcome(x, w, kernel.AttemptFailureFor(kernel.ClassConnect))
		return
	}
	var body []byte
	var renderFailure *media.Error
	if record.StrictContract {
		body, renderFailure = media.EncodeStrictVideoObject(result.Source, upstream, updated.ID, updated.RouteSlug)
	} else {
		body, renderFailure = media.EncodeVideoObject(kernel.MediaVideo(result), updated.ID, updated.RouteSlug)
	}
	if renderFailure != nil {
		s.kernel.FailMediaOutcome(x, w, kernel.AttemptFailureFor(kernel.ClassProtocol))
		return
	}
	out := kernel.MediaResultOutcome(result, http.StatusOK)
	s.kernel.DeliverMediaJSON(w, x, out, body)
	s.kernel.FinishMedia(x, out, out.Status())
}

func (s surface) videoContent(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.BeginMedia(w, r)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	x.UnaryFamily(wire.VideoContent)
	record, e := s.kernel.OwnedJob(r.Context(), authority, r.PathValue("video_id"), media.OpVideoContent)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if e = kernel.StrictVideoResourceContext(x, record, true); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	x.Route = kernel.VideoJobRoute(record)
	ctx, complete, admissionError := s.kernel.AdmitVideoRequest(r.Context(), x, authority)
	if admissionError != nil {
		s.kernel.Refuse(x, w, admissionError)
		return
	}
	defer complete()
	r = r.WithContext(ctx)
	variant, failure := media.ValidateVideoContentQuery(r.URL.Query())
	if failure != nil {
		s.kernel.Refuse(x, w, kernel.MediaError(failure))
		return
	}
	upstream, ok := record.Upstream()
	if !ok {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "media_job_upstream_id_unavailable", "The video job's upstream identity is unavailable."))
		return
	}
	result, transportFailure, fact := s.kernel.VideoJobCall(r.Context(), x, record, media.VideoContent{ID: upstream, Variant: variant})
	x.NoteDispatchFailure(transportFailure)
	x.NoteJobAttempt(fact)
	if transportFailure != nil {
		s.kernel.FailMediaOutcome(x, w, transportFailure)
		return
	}
	artifact := kernel.MediaArtifact(result.Payload)
	if artifact == nil {
		s.kernel.FailMediaOutcome(x, w, kernel.AttemptFailureFor(kernel.ClassProtocol))
		return
	}
	out := kernel.MediaResultOutcome(result, http.StatusOK)
	s.kernel.StreamMediaArtifact(w, x, out, artifact)
	s.kernel.FinishMedia(x, out, out.Status())
}

func (s surface) videoDelete(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.BeginMedia(w, r)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	x.UnaryFamily(wire.VideoDelete)
	record, e := s.kernel.OwnedJob(r.Context(), authority, r.PathValue("video_id"), media.OpVideoDelete)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if e = kernel.StrictVideoResourceContext(x, record, false); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	x.Route = kernel.VideoJobRoute(record)
	ctx, complete, admissionError := s.kernel.AdmitVideoRequest(r.Context(), x, authority)
	if admissionError != nil {
		s.kernel.Refuse(x, w, admissionError)
		return
	}
	defer complete()
	if record.Lifecycle == jobs.Deleted {
		s.repeatVideoDeletion(w, x, record)
		return
	}
	upstream, ok := record.Upstream()
	if !ok {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "media_job_upstream_id_unavailable", "The video job's upstream identity is unavailable."))
		return
	}
	result, transportFailure, fact := s.kernel.VideoJobCall(ctx, x, record, media.VideoDelete{ID: upstream})
	x.NoteDispatchFailure(transportFailure)
	x.NoteJobAttempt(fact)
	if undeleted := kernel.VideoUndeleted(result, transportFailure); undeleted != nil {
		s.kernel.FailMediaOutcome(x, w, undeleted)
		return
	}
	finalized, err := s.kernel.Deps.Media.Store.Finalize(ctx, record.ID, jobs.Unfenced)
	if err != nil {
		s.kernel.Refuse(x, w, kernel.JobError(err))
		return
	}
	if finalized.Lifecycle != jobs.Deleted {
		s.kernel.Deps.Media.Jobs.RecordGap()
		s.kernel.FailMediaOutcome(x, w, kernel.AttemptFailureFor(kernel.ClassConnect))
		return
	}
	if record.StrictContract && result == nil {
		s.kernel.FailMediaOutcome(x, w, transportFailure)
		return
	}
	body, renderFailure := kernel.VideoDeletion(record, upstream, result)
	if renderFailure != nil {
		s.kernel.FailMediaOutcome(x, w, kernel.AttemptFailureFor(kernel.ClassProtocol))
		return
	}
	out := kernel.MediaResultOutcome(result, http.StatusOK)
	s.kernel.DeliverMediaJSON(w, x, out, body)
	s.kernel.FinishMedia(x, out, out.Status())
}

func (s surface) repeatVideoDeletion(w http.ResponseWriter, x *kernel.Call, record *jobs.Record) {
	if record.StrictContract {
		s.kernel.Refuse(x, w, fault.NotFoundError("video_not_found", "The video job does not exist."))
		return
	}
	body, failure := media.EncodeVideoDeleteResponse(&media.VideoDeleteResult{ID: record.ID, Deleted: true}, record.ID)
	if failure != nil {
		s.kernel.Refuse(x, w, kernel.MediaError(failure))
		return
	}
	out := kernel.MediaStatusOutcome(http.StatusOK)
	x.MarkDispatched()
	s.kernel.DeliverMediaJSON(w, x, out, body)
	s.kernel.FinishMedia(x, out, out.Status())
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
