package gateway

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func (s *Server) videoList(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.mediaBegin(w, r)
	if done {
		return
	}
	defer s.gate.Release(r.Context())
	x.family = wire.VideoList
	x.mode = "unary"
	ctx, complete, admissionError := s.admitVideoRequest(r.Context(), x, authority)
	if admissionError != nil {
		s.refuse(x, w, admissionError)
		return
	}
	defer complete()
	r = r.WithContext(ctx)
	query, failure := media.ValidateVideoListQuery(r.URL.Query())
	if failure != nil {
		s.refuse(x, w, mediaError(failure))
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
	after, err := s.deps.Media.Store.Position(r.Context(), cursorUUID(query.After), listing)
	if err != nil {
		s.refuse(x, w, jobError(err))
		return
	}
	listing.After = after
	page, err := s.deps.Media.Store.List(r.Context(), listing)
	if err != nil {
		s.refuse(x, w, jobError(err))
		return
	}
	refreshed := make([]jobs.Record, 0, len(page.Items))
	var refreshError *fault.Error
	for _, record := range page.Items {
		updated, fact, dispatched, e := s.refreshListRecord(r.Context(), x, record)
		refreshed = append(refreshed, updated)
		x.dispatched = x.dispatched || dispatched
		if fact != nil {
			x.facts = append(x.facts, *fact)
		}
		if e != nil {
			refreshError = e
			break
		}
	}
	x.notePolls()
	if refreshError != nil {
		s.refuse(x, w, refreshError)
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
	body, failure := media.EncodeVideoListResponse(list, "video")
	if failure != nil {
		s.refuse(x, w, fault.ServerError(http.StatusBadGateway, failure.Code, failure.Message))
		return
	}
	x.dispatched = true
	out := &mediaOutcome{committed: true, status: http.StatusOK}
	s.deliverMediaJSON(w, x, out, body)
	s.finishMedia(x, out, out.status)
}

func (s *Server) refreshListRecord(ctx context.Context, x *call, record jobs.Record) (jobs.Record, *events.AttemptFact, bool, *fault.Error) {
	upstream, ok := record.Upstream()
	if !record.State.Polling() || !ok {
		return record, nil, false, nil
	}
	if record.StrictContract {
		if _, current, e := s.jobRoute(ctx, record.RouteSlug); e != nil || retainedContract(true, current, retainedRetrieval) != nil {
			return record, nil, false, nil
		}
	}
	result, transportFailure, fact := s.videoJobCall(ctx, x, &record, media.VideoGet{ID: upstream})
	if transportFailure != nil {
		if transportFailure.quota != "" || transportFailure.class == classLimitsUnavailable {
			return record, &fact, false, transportFailure.toError()
		}
		return record, &fact, transportFailure.dispatched, nil
	}
	update, ok := jobs.Observe(mediaVideo(result), s.now())
	if !ok {
		return record, &fact, true, nil
	}
	updated, err := s.deps.Media.Store.Refresh(ctx, record.ID, jobs.Unfenced, update)
	if err != nil {
		return record, &fact, true, nil
	}
	return updated, &fact, true, nil
}

func (s *Server) videoGet(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.mediaBegin(w, r)
	if done {
		return
	}
	defer s.gate.Release(r.Context())
	x.family = wire.VideoGet
	x.mode = "unary"
	record, e := s.ownedJob(r.Context(), authority, r.PathValue("video_id"), media.OpVideoGet)
	if e != nil {
		s.refuse(x, w, e)
		return
	}
	if e = strictVideoResourceContext(x, record, false); e != nil {
		s.refuse(x, w, e)
		return
	}
	x.route = videoJobRoute(record)
	ctx, complete, admissionError := s.admitVideoRequest(r.Context(), x, authority)
	if admissionError != nil {
		s.refuse(x, w, admissionError)
		return
	}
	defer complete()
	r = r.WithContext(ctx)
	upstream, ok := record.Upstream()
	if !ok {
		s.refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "media_job_upstream_id_unavailable", "The video job's upstream identity is unavailable."))
		return
	}
	result, transportFailure, fact := s.videoJobCall(r.Context(), x, record, media.VideoGet{ID: upstream})
	x.dispatched = transportFailure == nil || transportFailure.dispatched
	x.noteJobAttempt(fact)
	if transportFailure != nil {
		s.mediaFailOutcome(x, w, transportFailure)
		return
	}
	update, ok := jobs.Observe(mediaVideo(result), s.now())
	if !ok {
		s.mediaFailOutcome(x, w, &attemptFailure{class: classProtocol})
		return
	}
	updated, err := s.deps.Media.Store.Refresh(r.Context(), record.ID, jobs.Unfenced, update)
	if err != nil {
		s.mediaFailOutcome(x, w, &attemptFailure{class: classConnect})
		return
	}
	var body []byte
	var renderFailure *media.Error
	if record.StrictContract {
		body, renderFailure = media.EncodeStrictVideoObject(result.Source, upstream, updated.ID, updated.RouteSlug)
	} else {
		body, renderFailure = media.EncodeVideoObject(mediaVideo(result), updated.ID, updated.RouteSlug)
	}
	if renderFailure != nil {
		s.mediaFailOutcome(x, w, &attemptFailure{class: classProtocol})
		return
	}
	out := &mediaOutcome{result: result, committed: true, status: http.StatusOK}
	s.deliverMediaJSON(w, x, out, body)
	s.finishMedia(x, out, out.status)
}

func (s *Server) videoContent(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.mediaBegin(w, r)
	if done {
		return
	}
	defer s.gate.Release(r.Context())
	x.family = wire.VideoContent
	x.mode = "unary"
	record, e := s.ownedJob(r.Context(), authority, r.PathValue("video_id"), media.OpVideoContent)
	if e != nil {
		s.refuse(x, w, e)
		return
	}
	if e = strictVideoResourceContext(x, record, true); e != nil {
		s.refuse(x, w, e)
		return
	}
	x.route = videoJobRoute(record)
	ctx, complete, admissionError := s.admitVideoRequest(r.Context(), x, authority)
	if admissionError != nil {
		s.refuse(x, w, admissionError)
		return
	}
	defer complete()
	r = r.WithContext(ctx)
	variant, failure := media.ValidateVideoContentQuery(r.URL.Query())
	if failure != nil {
		s.refuse(x, w, mediaError(failure))
		return
	}
	upstream, ok := record.Upstream()
	if !ok {
		s.refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "media_job_upstream_id_unavailable", "The video job's upstream identity is unavailable."))
		return
	}
	result, transportFailure, fact := s.videoJobCall(r.Context(), x, record, media.VideoContent{ID: upstream, Variant: variant})
	x.dispatched = transportFailure == nil || transportFailure.dispatched
	x.noteJobAttempt(fact)
	if transportFailure != nil {
		s.mediaFailOutcome(x, w, transportFailure)
		return
	}
	artifact := mediaArtifact(result.Payload)
	if artifact == nil {
		s.mediaFailOutcome(x, w, &attemptFailure{class: classProtocol})
		return
	}
	out := &mediaOutcome{result: result, committed: true, status: http.StatusOK}
	s.streamArtifact(w, x, out, artifact)
	s.finishMedia(x, out, out.status)
}

func (s *Server) videoDelete(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.mediaBegin(w, r)
	if done {
		return
	}
	defer s.gate.Release(r.Context())
	x.family = wire.VideoDelete
	x.mode = "unary"
	loaded, e := s.ownedJob(r.Context(), authority, r.PathValue("video_id"), media.OpVideoDelete)
	if e != nil {
		s.refuse(x, w, e)
		return
	}
	if e = strictVideoResourceContext(x, loaded, false); e != nil {
		s.refuse(x, w, e)
		return
	}
	x.route = videoJobRoute(loaded)
	ctx, complete, admissionError := s.admitVideoRequest(r.Context(), x, authority)
	if admissionError != nil {
		s.refuse(x, w, admissionError)
		return
	}
	defer complete()
	r = r.WithContext(ctx)
	record := *loaded
	if record.Lifecycle == jobs.Deleted {
		if record.StrictContract {
			s.refuse(x, w, fault.NotFoundError("video_not_found", "The video job does not exist."))
			return
		}
		body, failure := media.EncodeVideoDeleteResponse(&media.VideoDeleteResult{ID: record.ID, Deleted: true}, record.ID)
		if failure != nil {
			s.refuse(x, w, mediaError(failure))
			return
		}
		out := &mediaOutcome{committed: true, status: http.StatusOK}
		x.dispatched = true
		s.deliverMediaJSON(w, x, out, body)
		s.finishMedia(x, out, out.status)
		return
	}
	upstream, ok := record.Upstream()
	if !ok {
		s.refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "media_job_upstream_id_unavailable", "The video job's upstream identity is unavailable."))
		return
	}
	result, transportFailure, fact := s.videoJobCall(r.Context(), x, &record, media.VideoDelete{ID: upstream})
	x.dispatched = transportFailure == nil || transportFailure.dispatched
	x.noteJobAttempt(fact)
	deleted := false
	if transportFailure != nil {
		if transportFailure.status != 404 {
			s.mediaFailOutcome(x, w, transportFailure)
			return
		}
		deleted = true
	} else {
		receipt := mediaDeleted(result)
		if receipt == nil {
			s.mediaFailOutcome(x, w, &attemptFailure{class: classProtocol})
			return
		}
		deleted = receipt.Deleted
	}
	if !deleted {
		s.mediaFailOutcome(x, w, &attemptFailure{class: classUpstreamClient, status: http.StatusBadGateway})
		return
	}
	finalized, err := s.deps.Media.Store.Finalize(r.Context(), record.ID, jobs.Unfenced)
	if err != nil {
		s.refuse(x, w, jobError(err))
		return
	}
	if finalized.Lifecycle != jobs.Deleted {
		s.deps.Media.Jobs.RecordGap()
		s.mediaFailOutcome(x, w, &attemptFailure{class: classConnect})
		return
	}
	if record.StrictContract && result == nil {
		s.mediaFailOutcome(x, w, transportFailure)
		return
	}
	var body []byte
	var renderFailure *media.Error
	if record.StrictContract {
		body, renderFailure = media.EncodeStrictVideoObject(result.Source, upstream, record.ID, "")
	} else {
		local := ""
		var extra map[string]any
		if receipt := mediaDeleted(result); receipt != nil {
			extra = receipt.Extra
		}
		body, renderFailure = media.EncodeVideoDeleteResponse(&media.VideoDeleteResult{ID: local, Deleted: true, Extra: extra}, record.ID)
	}
	if renderFailure != nil {
		s.mediaFailOutcome(x, w, &attemptFailure{class: classProtocol})
		return
	}
	out := &mediaOutcome{result: result, committed: true, status: http.StatusOK}
	s.deliverMediaJSON(w, x, out, body)
	s.finishMedia(x, out, out.status)
}

func (s *Server) mediaFailOutcome(x *call, w http.ResponseWriter, failure *attemptFailure) {
	e := failure.toError()
	s.finishMedia(x, &mediaOutcome{err: e, committed: failure.committed}, e.Status)
	fault.WriteSurfaceError(w, e, "openai")
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
