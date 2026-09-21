package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

// videoCreate reserves a durable local job before the non-idempotent upstream
// create is attempted, then completes the two-phase attach.
func (s *Server) videoCreate(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.mediaBegin(w, r)
	if done {
		return
	}
	form, parseError := s.parseMediaForm(w, r, authority.ID, media.DefaultVideoReferenceLimit, 1)
	if parseError != nil {
		s.release(r.Context())
		s.mediaFail(x, w, parseError)
		return
	}
	request, failure := media.DecodeVideoCreate(form)
	if failure != nil {
		form.Cleanup()
		s.release(r.Context())
		s.mediaFail(x, w, mediaError(failure))
		return
	}
	x.family = openai.Family(request.Op)
	x.media = request
	x.estimate = mediaMultipartTokens
	defer s.release(r.Context())
	defer s.cleanupUploads(request)

	localJobID := uuid.Must(uuid.NewV7()).String()
	x.affinity = []byte(localJobID)
	if e := s.prepareMedia(x, authority); e != nil {
		form.Cleanup()
		s.mediaFail(x, w, e)
		return
	}
	if e := s.enforceMediaInput(x, request); e != nil {
		form.Cleanup()
		s.mediaFail(x, w, e)
		return
	}
	form.Disarm()
	// The reservation pins exactly one provider target; create runs on that
	// target alone because a second provider would mint a second job.
	attempt := x.attempts[0]
	provider, ok := x.request.release.Snapshot.Providers[attempt.ProviderID]
	if !ok {
		s.mediaFail(x, w, serverError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider is currently able to serve `"+x.route.Slug+"`."))
		return
	}
	slots := s.slots(x, attempt, &provider)
	if len(slots) == 0 {
		s.mediaFail(x, w, serverError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider credential is currently able to serve `"+x.route.Slug+"`."))
		return
	}

	overall := time.Duration(x.route.OverallTimeout) * time.Millisecond
	ctx, cancel := context.WithTimeout(r.Context(), overall)
	defer cancel()
	var e *Error
	if x.lease, e = s.Admission.reserveKey(ctx, authority, x.estimate, overall); e != nil {
		s.mediaFail(x, w, e)
		return
	}
	defer func() { settleKey(ctx, x.lease, x.dispatched, x.settledTokens(), s.log) }()

	deadline, _ := ctx.Deadline()
	hold, slot, e := s.admitVideoSlot(ctx, x, attempt, &provider, slots, deadline)
	if hold == nil {
		s.mediaFail(x, w, e)
		return
	}
	defer func() { hold.settle(ctx, x.dispatched, x.settledTokens()) }()

	reserved, jobErr := media.ReserveJob(ctx, s.Media.Jobs.Pool, media.Reservation{
		ID:                  localJobID,
		RuntimeGenerationID: x.request.release.Snapshot.Generation.ID,
		ProviderRevisionID:  attempt.ProviderRevisionID,
		APIKeyID:            authority.ID,
		ProviderID:          attempt.ProviderID,
		UpstreamModel:       attempt.UpstreamModel,
		RouteSlug:           x.route.Slug,
		Operation:           media.OpVideoCreate,
		Surface:             "openai",
		CredentialVersionID: slot.CredentialID,
		SlotID:              &slot.ID,
	})
	if jobErr != nil {
		// A local persistence failure before dispatch refunds the quota
		// reservation and returns any half-open probe it holds.
		s.releaseHold(ctx, hold)
		s.mediaFail(x, w, mediaError(media.JobHTTPError(jobErr)))
		return
	}

	// The accepted upstream create must outlive a client disconnect: run the
	// dispatch on a detached context bounded by the route deadline.
	dispatchCtx, dispatchCancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer dispatchCancel()
	fact, result, dispatchFailure := s.mediaAttempt(dispatchCtx, w, x, attempt, &provider, slot, len(x.facts)+1)
	x.dispatched = dispatchFailure == nil || dispatchFailure.dispatched
	x.facts = append(x.facts, fact)
	s.health.record(provider.ID, fact)
	if dispatchFailure != nil {
		s.retireFailedCreate(dispatchCtx, reserved.ID, dispatchFailure)
		out := &mediaOutcome{err: dispatchFailure.toError(), committed: dispatchFailure.committed, cancelled: dispatchFailure.class == classCancelled}
		s.finishMedia(x, out, out.err.Status)
		writeSurfaceError(w, out.err, "openai")
		return
	}
	out := s.attachCreated(dispatchCtx, x, reserved, result, localJobID)
	if out.err != nil {
		s.finishMedia(x, out, out.err.Status)
		writeSurfaceError(w, out.err, "openai")
		return
	}
	s.writeMediaResult(w, x, out)
	s.finishMedia(x, out, out.status)
}

// admitVideoSlot gates the durable create's candidate slots through the
// shared pre-dispatch boundary until one admits. The pinned single-dispatch
// semantics are unchanged: only local pre-dispatch outcomes — a revoked or
// cooling credential, an unmeterable target, a quota refusal — advance to a
// sibling, and the first admitted slot is the one the durable reservation
// pins. It returns the error to report when no slot admits.
func (s *Server) admitVideoSlot(ctx context.Context, x *execution, attempt runtime.Attempt, provider *runtime.Provider, slots []runtime.Slot, deadline time.Time) (*dispatchHold, runtime.Slot, *Error) {
	used := 0
	unmeterable := false
	var last *attemptFailure
	for _, slot := range slots {
		if used >= x.budget || ctx.Err() != nil {
			break
		}
		gate := s.gateSlot(ctx, provider, &slot, x.estimate, deadline)
		switch gate.verdict {
		case gateExpired:
			return nil, runtime.Slot{}, (&attemptFailure{class: classTimeout}).toError()
		case gateDenied:
			// The circuit refused the probe; siblings share the endpoint.
			return nil, runtime.Slot{}, s.videoAdmissionError(ctx, x, last, unmeterable)
		case gateRejected:
			used++
			x.facts = append(x.facts, s.rejectedFact(x, attempt, slot, used, gate.rejection))
			last = gate.rejection
			if gate.rejection.quota == quotaConnection {
				return nil, runtime.Slot{}, s.videoAdmissionError(ctx, x, last, unmeterable)
			}
		case gateUnmeterable:
			unmeterable = true
		case gateAdmitted:
			return gate.hold, slot, nil
		}
	}
	return nil, runtime.Slot{}, s.videoAdmissionError(ctx, x, last, unmeterable)
}

// videoAdmissionError renders the terminal error when no candidate slot could
// be admitted, matching the precedence the media attempt loop uses.
func (s *Server) videoAdmissionError(ctx context.Context, x *execution, last *attemptFailure, unmeterable bool) *Error {
	switch {
	case ctx.Err() != nil && errors.Is(context.Cause(ctx), context.DeadlineExceeded):
		return (&attemptFailure{class: classTimeout}).toError()
	case ctx.Err() != nil:
		return (&attemptFailure{class: classCancelled}).toError()
	case last != nil:
		return last.toError()
	case unmeterable:
		return limitsUnavailable()
	}
	return serverError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider credential is currently able to serve `"+x.route.Slug+"`.")
}

// retireFailedCreate maps a failed create dispatch onto the reservation: an
// ambiguous outcome waits for reconciliation, a definitive rejection retires
// the row immediately.
func (s *Server) retireFailedCreate(ctx context.Context, id string, failure *attemptFailure) {
	if failure.class == classAmbiguous {
		if _, err := media.MarkCreateAmbiguous(ctx, s.Media.Jobs.Pool, id, "upstream_create_result_ambiguous"); err != nil {
			s.Media.Jobs.Log.Error("failed to mark ambiguous video creation", "job_id", id, "error", err)
		}
		return
	}
	finalized, err := s.Media.Jobs.FinalizeDeletion(ctx, id)
	if err != nil || !finalized {
		s.Media.Jobs.RecordGap()
		s.Media.Jobs.Log.Error("abandoned video reservation was not finalized", "job_id", id, "error", err)
	}
}

// attachCreated validates the provider reply and binds the upstream identity.
func (s *Server) attachCreated(ctx context.Context, x *execution, reserved media.JobRecord, result *media.Result, localJobID string) *mediaOutcome {
	out := &mediaOutcome{result: result, committed: true, status: http.StatusCreated, localJob: localJobID}
	protocol := func(message string) *mediaOutcome {
		out.err = serverError(http.StatusBadGateway, "provider_protocol_error", message)
		return out
	}
	if result.Video == nil {
		if _, err := media.MarkCreateAmbiguous(ctx, s.Media.Jobs.Pool, reserved.ID, "upstream_create_response_missing_job_identity"); err != nil {
			s.Media.Jobs.RecordGap()
			s.Media.Jobs.Log.Error("failed to retire malformed video reservation", "job_id", reserved.ID, "error", err)
		}
		return protocol("The provider returned an incompatible video creation response.")
	}
	upstreamID := result.Video.ID
	if !media.ValidUpstreamJobID(upstreamID) {
		if _, err := media.MarkCreateAmbiguous(ctx, s.Media.Jobs.Pool, reserved.ID, "upstream_create_response_invalid_job_identity"); err != nil {
			s.Media.Jobs.RecordGap()
			s.Media.Jobs.Log.Error("failed to retire invalid video reservation", "job_id", reserved.ID, "error", err)
		}
		return protocol("The provider returned an invalid video job identity.")
	}
	state, ok := media.VideoState(result.Video.Status)
	if !ok {
		if _, err := media.MarkCreateCleanupPending(ctx, s.Media.Jobs.Pool, reserved.ID, upstreamID, "upstream_create_response_invalid_status"); err != nil {
			s.Media.Jobs.RecordGap()
			s.Media.Jobs.Log.Error("failed to schedule malformed video cleanup", "job_id", reserved.ID, "error", err)
		}
		return protocol("The provider returned an unsupported video status.")
	}
	update := media.JobUpdate{
		State:            state,
		ProgressPercent:  result.Video.Progress,
		ContentAvailable: result.Video.Status == "completed",
		ExpiresAt:        media.UnixTime(result.Video.ExpiresAt),
		ErrorClass:       result.Video.ErrorCode,
		LastPolledAt:     s.now(),
	}
	record, err := s.Media.Jobs.AttachWithRetry(ctx, reserved.ID, upstreamID, update)
	if err != nil {
		return s.handleFailedAttachment(ctx, x, reserved, upstreamID, result, err, out)
	}
	_ = record
	return out
}

// handleFailedAttachment persists cleanup intent, runs the compensation
// delete, and finalizes the tombstone when the provider confirms.
func (s *Server) handleFailedAttachment(ctx context.Context, x *execution, reservedRecord media.JobRecord, upstreamID string, result *media.Result, attachErr error, out *mediaOutcome) *mediaOutcome {
	conflict := false
	var jobErr *media.JobError
	if errors.As(attachErr, &jobErr) && jobErr.Kind == media.JobErrorUpstreamIdentityConflict {
		conflict = true
	}
	intentPersisted := false
	if !conflict {
		if record, err := media.MarkCreateCleanupPending(ctx, s.Media.Jobs.Pool, reservedRecord.ID, upstreamID, "upstream_created_local_attach_failed"); err == nil &&
			record.Lifecycle == media.LifecycleCreateCleanupPending &&
			record.UpstreamJobID != nil && *record.UpstreamJobID == upstreamID {
			intentPersisted = true
		} else if err != nil {
			s.Media.Jobs.Log.Error("failed to persist video cleanup metadata", "job_id", reservedRecord.ID, "error", err)
		}
	}
	compensated := false
	if intentPersisted {
		compensated = s.compensateCreate(ctx, reservedRecord, upstreamID)
	}
	if compensated {
		finalized, err := s.Media.Jobs.FinalizeDeletion(ctx, reservedRecord.ID)
		if err != nil || !finalized {
			s.Media.Jobs.RecordGap()
			s.Media.Jobs.Log.Error("upstream cleanup succeeded but tombstone was not finalized", "job_id", reservedRecord.ID, "error", err)
		}
	} else {
		s.Media.Jobs.RecordGap()
		s.Media.Jobs.Log.Error("video create reconciliation gap requires operator attention",
			"job_id", reservedRecord.ID, "upstream_job_id", upstreamID, "provider_id", x.media.Route)
	}
	out.err = serverError(http.StatusServiceUnavailable, "media_job_create_reconciliation_pending", "The video creation is being reconciled; check the job status shortly.")
	return out
}

// compensateCreate issues the upstream delete that retires an orphaned job.
// A missing upstream object is a successful compensation.
func (s *Server) compensateCreate(ctx context.Context, reserved media.JobRecord, upstreamID string) bool {
	target, routeTimeout, _ := s.Media.Jobs.JobTarget(ctx, &reserved)
	if target == nil {
		return false
	}
	call, failure := media.Encode(&media.Request{Op: media.OpVideoDelete, JobID: upstreamID, Route: reserved.RouteSlug}, target.Target.Config.Kind, target.Model)
	if failure != nil {
		return false
	}
	callCtx, cancel := context.WithTimeout(ctx, routeTimeout+media.ReconciliationLeaseSlack)
	defer cancel()
	result, transportFailure := s.Media.Jobs.Transport.Do(callCtx, target.Target, call, nil)
	if transportFailure != nil {
		return transportFailure.Status == 404
	}
	return result.Deleted != nil && result.Deleted.Deleted
}

// ownedJob loads a media job the caller is allowed to see, enforcing
// ownership, lifecycle visibility, and the route permission.
func (s *Server) ownedJob(ctx context.Context, authority access.Authority, videoID, op string) (*media.JobRecord, *Error) {
	id, err := uuid.Parse(videoID)
	if err != nil {
		return nil, notFoundError("video_not_found", "The video job does not exist.")
	}
	record, err := media.Job(ctx, s.Media.Jobs.Pool, id.String())
	if err != nil {
		return nil, mediaError(media.JobHTTPError(err))
	}
	if record.APIKeyID != authority.ID {
		return nil, notFoundError("video_not_found", "The video job does not exist.")
	}
	if record.Lifecycle == media.LifecycleDeleted && op != media.OpVideoDelete {
		return nil, notFoundError("video_not_found", "The video job does not exist.")
	}
	switch record.Lifecycle {
	case media.LifecycleActive, media.LifecycleDeletePending, media.LifecycleDeleted:
	default:
		return nil, serverError(http.StatusServiceUnavailable, "media_job_reconciliation_pending", "The video job is being reconciled; retry shortly.")
	}
	if !authority.Allows("inference", record.RouteSlug, s.now()) {
		return nil, permissionError("route_forbidden", "This API key is not allowed to use the model `"+record.RouteSlug+"`.")
	}
	return &record, nil
}

// jobTarget resolves the pinned upstream target for a media job operation.
func (s *Server) jobTarget(ctx context.Context, record *media.JobRecord) (*media.JobTarget, time.Duration, *Error) {
	target, timeout, code := s.Media.Jobs.JobTarget(ctx, record)
	if target == nil {
		return nil, 0, serverError(http.StatusServiceUnavailable, code, "The media job's provider target is unavailable.")
	}
	return target, timeout, nil
}

// admitVideoRequest applies key budgets once, before job reads or mutations.
func (s *Server) admitVideoRequest(parent context.Context, x *execution, authority access.Authority) (context.Context, func(), *Error) {
	ttl := maxStreamDuration + media.ReconciliationLeaseSlack
	x.budgetGroupID = authority.BudgetGroupID
	ctx, cancel := context.WithTimeout(parent, ttl)
	var e *Error
	x.lease, e = s.Admission.reserveKey(ctx, authority, 0, ttl)
	if e != nil {
		cancel()
		return nil, nil, e
	}
	return ctx, func() {
		settleKey(ctx, x.lease, x.dispatched, x.settledTokens(), s.log)
		cancel()
	}, nil
}

// videoJobCall dispatches one pinned-target upstream call for a media job and
// returns the attempt fact for the caller to record.
func (s *Server) videoJobCall(ctx context.Context, x *execution, record *media.JobRecord, call *media.UpstreamCall, request *media.Request) (*media.Result, *attemptFailure, AttemptFact) {
	fact := AttemptFact{
		TargetID:           record.ID,
		ProviderID:         record.ProviderID,
		ProviderRevisionID: record.ProviderRevisionID,
		UpstreamModel:      record.UpstreamModel,
		Mode:               "unary",
		StartedAt:          s.now(),
	}
	if record.CredentialVersionID != nil {
		fact.CredentialID = *record.CredentialVersionID
	}
	target, timeout, e := s.jobTarget(ctx, record)
	if e != nil {
		fact.Class = classConnect
		fact.Duration = s.now().Sub(fact.StartedAt)
		fact.recordEvidence(false)
		return nil, &attemptFailure{class: classConnect}, fact
	}
	// Connection and credential authority stay pinned; quota changes in the
	// current release still apply to subsequent requests for the retained job.
	provider, slot := target.Provider, target.Slot
	if current, ok := x.request.release.Snapshot.Providers[provider.ID]; ok {
		provider.Limits = current.Limits
		for _, currentSlot := range current.Slots {
			if currentSlot.ID == slot.ID {
				slot.RequestsPerMinute = currentSlot.RequestsPerMinute
				slot.TokensPerMinute = currentSlot.TokensPerMinute
				slot.MaxConcurrency = currentSlot.MaxConcurrency
				break
			}
		}
	}
	fact.SlotID = slot.ID
	reservation, rejection, skip := s.Admission.reserveTarget(ctx, &provider, &slot, 0, timeout+media.ReconciliationLeaseSlack)
	if skip {
		rejection = &attemptFailure{class: classLimitsUnavailable}
	}
	if rejection != nil {
		fact.Class = rejection.class
		fact.Duration = s.now().Sub(fact.StartedAt)
		fact.recordEvidence(false)
		return nil, rejection, fact
	}
	dispatched := false
	defer func() { reservation.settle(ctx, dispatched, nil) }()
	if request.Op == media.OpVideoDelete {
		if _, err := media.BeginDeletion(ctx, s.Media.Jobs.Pool, record.ID); err != nil {
			fact.Class = classConnect
			fact.Duration = s.now().Sub(fact.StartedAt)
			fact.recordEvidence(false)
			return nil, &attemptFailure{class: classConnect}, fact
		}
	}
	attemptCtx, atr := x.request.trace.Attempt(ctx, string(target.Config.Kind), record.ProviderRevisionID, record.UpstreamModel)
	if x.request.trace.PropagateUpstream() {
		call.Inject = http.Header{}
		atr.InjectUpstream(call.Inject, true)
	}
	callCtx, cancel := context.WithTimeout(attemptCtx, timeout+media.ReconciliationLeaseSlack)
	defer cancel()
	result, failure := s.Media.Jobs.Transport.Do(callCtx, target.Target, call, request)
	dispatched = failure == nil || failure.Dispatched
	fact.Duration = s.now().Sub(fact.StartedAt)
	if failure != nil {
		fact.Class = mediaClass(failure)
		fact.Status = failure.Status
		if failure.RetryAfter > 0 {
			retry := failure.RetryAfter
			fact.RetryAfter = &retry
		}
		f := &attemptFailure{
			class:      mediaClass(failure),
			status:     failure.Status,
			retryAfter: failure.RetryAfter,
			upstream:   failure.Upstream,
			dispatched: failure.Dispatched,
		}
		fact.recordEvidence(f.billingUncertain())
		s.health.record(record.ProviderID, fact)
		atr.Finish(fact.Class, fact.Status)
		return nil, f, fact
	}
	fact.Class = "success"
	fact.Status = result.Status
	fb := result.FirstByte
	fact.FirstByte = &fb
	fact.Usage = mediaUsage(request, result)
	fact.Committed = true
	fact.recordEvidence(false)
	s.health.record(record.ProviderID, fact)
	if u := fact.Usage; u != nil {
		atr.RecordUsage(&u.InputTokens, &u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
	}
	atr.Finish(fact.Class, fact.Status)
	return result, nil, fact
}

// videoList pages the caller's jobs and refreshes non-terminal records.
func (s *Server) videoList(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.mediaBegin(w, r)
	if done {
		return
	}
	defer s.release(r.Context())
	x.family = openai.FamilyVideoList
	x.mode = "unary"
	ctx, complete, admissionError := s.admitVideoRequest(r.Context(), x, authority)
	if admissionError != nil {
		s.mediaFail(x, w, admissionError)
		return
	}
	defer complete()
	r = r.WithContext(ctx)
	query, failure := media.ValidateVideoListQuery(r.URL.Query())
	if failure != nil {
		s.mediaFail(x, w, mediaError(failure))
		return
	}
	page, err := media.JobsAfterID(r.Context(), s.Media.Jobs.Pool, media.Filters{
		APIKeyID:   &authority.ID,
		RouteSlugs: authority.Policy.AllowedRoutes,
		Operation:  new(media.OpVideoCreate),
		Surface:    new("openai"),
	}, cursorUUID(query.After), query.Order, query.Limit)
	if err != nil {
		s.mediaFail(x, w, mediaError(media.JobHTTPError(err)))
		return
	}
	refreshed := make([]media.JobRecord, len(page.Items))
	type refreshResult struct {
		index      int
		record     media.JobRecord
		fact       *AttemptFact
		dispatched bool
		err        *Error
	}
	results := make(chan refreshResult, len(page.Items))
	for i, record := range page.Items {
		i, record := i, record
		go func() {
			updated, fact, dispatched, e := s.refreshListRecord(r.Context(), x, record)
			results <- refreshResult{i, updated, fact, dispatched, e}
		}()
	}
	var refreshError *Error
	for range page.Items {
		outcome := <-results
		refreshed[outcome.index] = outcome.record
		if outcome.err != nil {
			refreshError = outcome.err
		}
		x.dispatched = x.dispatched || outcome.dispatched
		if outcome.fact != nil {
			x.facts = append(x.facts, *outcome.fact)
		}
	}
	if refreshError != nil {
		s.mediaFail(x, w, refreshError)
		return
	}
	jobs := make([]media.VideoJobResult, 0, len(refreshed))
	for _, record := range refreshed {
		jobs = append(jobs, mediaJobResult(&record))
	}
	var firstID, lastID *string
	if len(jobs) > 0 {
		first := jobs[0].ID
		last := jobs[len(jobs)-1].ID
		firstID, lastID = &first, &last
	}
	list := &media.VideoListResult{Jobs: jobs, FirstID: firstID, LastID: lastID, HasMore: page.NextCursor != nil}
	body, failure := media.EncodeVideoListResponse(list, "video")
	if failure != nil {
		s.mediaFail(x, w, serverError(http.StatusBadGateway, failure.Code, failure.Message))
		return
	}
	x.dispatched = true // A successful list consumes one key request even without polls.
	out := &mediaOutcome{committed: true, status: http.StatusOK}
	s.deliverMediaJSON(w, x, out, body)
	s.finishMedia(x, out, out.status)
}

// refreshListRecord polls a non-terminal job during a client list. The fact
// and the dispatch flag are returned for the serial collector to record.
func (s *Server) refreshListRecord(ctx context.Context, x *execution, record media.JobRecord) (media.JobRecord, *AttemptFact, bool, *Error) {
	if record.State != media.StateQueued && record.State != media.StateRunning {
		return record, nil, false, nil
	}
	if record.UpstreamJobID == nil || !media.ValidUpstreamJobID(*record.UpstreamJobID) {
		return record, nil, false, nil
	}
	call, failure := media.Encode(&media.Request{Op: media.OpVideoGet, JobID: *record.UpstreamJobID, Route: record.RouteSlug}, "openai", record.UpstreamModel)
	if failure != nil {
		return record, nil, false, nil
	}
	result, transportFailure, fact := s.videoJobCall(ctx, x, &record, call, &media.Request{Op: media.OpVideoGet, Route: record.RouteSlug})
	if transportFailure != nil {
		if transportFailure.quota != "" || transportFailure.class == classLimitsUnavailable {
			return record, &fact, false, transportFailure.toError()
		}
		return record, &fact, transportFailure.dispatched, nil
	}
	if result == nil || result.Video == nil {
		return record, &fact, true, nil
	}
	state, ok := media.VideoState(result.Video.Status)
	if !ok {
		return record, &fact, true, nil
	}
	updated, err := media.RefreshJob(ctx, s.Media.Jobs.Pool, record.ID, media.JobUpdate{
		State:            state,
		ProgressPercent:  result.Video.Progress,
		ContentAvailable: result.Video.Status == "completed",
		ExpiresAt:        media.UnixTime(result.Video.ExpiresAt),
		ErrorClass:       result.Video.ErrorCode,
		LastPolledAt:     s.now(),
	})
	if err != nil {
		return record, &fact, true, nil
	}
	return updated, &fact, true, nil
}

// videoGet refreshes and renders one owned job.
func (s *Server) videoGet(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.mediaBegin(w, r)
	if done {
		return
	}
	defer s.release(r.Context())
	x.family = openai.FamilyVideoGet
	x.mode = "unary"
	ctx, complete, admissionError := s.admitVideoRequest(r.Context(), x, authority)
	if admissionError != nil {
		s.mediaFail(x, w, admissionError)
		return
	}
	defer complete()
	r = r.WithContext(ctx)
	record, e := s.ownedJob(r.Context(), authority, r.PathValue("video_id"), media.OpVideoGet)
	if e != nil {
		s.mediaFail(x, w, e)
		return
	}
	if record.UpstreamJobID == nil || !media.ValidUpstreamJobID(*record.UpstreamJobID) {
		s.mediaFail(x, w, serverError(http.StatusServiceUnavailable, "media_job_upstream_id_unavailable", "The video job's upstream identity is unavailable."))
		return
	}
	x.route = &runtime.Route{Slug: record.RouteSlug}
	call, failure := media.Encode(&media.Request{Op: media.OpVideoGet, JobID: *record.UpstreamJobID, Route: record.RouteSlug}, "openai", record.UpstreamModel)
	if failure != nil {
		s.mediaFail(x, w, mediaError(failure))
		return
	}
	result, transportFailure, fact := s.videoJobCall(r.Context(), x, record, call, &media.Request{Op: media.OpVideoGet, Route: record.RouteSlug})
	x.dispatched = transportFailure == nil || transportFailure.dispatched
	x.facts = append(x.facts, fact)
	if transportFailure != nil {
		s.mediaFailOutcome(x, w, transportFailure)
		return
	}
	if result.Video == nil {
		s.mediaFailOutcome(x, w, &attemptFailure{class: classProtocol})
		return
	}
	state, ok := media.VideoState(result.Video.Status)
	if !ok {
		s.mediaFailOutcome(x, w, &attemptFailure{class: classProtocol})
		return
	}
	updated, err := media.RefreshJob(r.Context(), s.Media.Jobs.Pool, record.ID, media.JobUpdate{
		State:            state,
		ProgressPercent:  result.Video.Progress,
		ContentAvailable: result.Video.Status == "completed",
		ExpiresAt:        media.UnixTime(result.Video.ExpiresAt),
		ErrorClass:       result.Video.ErrorCode,
		LastPolledAt:     s.now(),
	})
	if err != nil {
		s.mediaFailOutcome(x, w, &attemptFailure{class: classConnect})
		return
	}
	result.Video.ID = updated.ID
	result.Video.Route = updated.RouteSlug
	body, failure := media.EncodeVideoObject(result.Video, updated.ID, updated.RouteSlug)
	if failure != nil {
		s.mediaFailOutcome(x, w, &attemptFailure{class: classProtocol})
		return
	}
	out := &mediaOutcome{result: result, committed: true, status: http.StatusOK}
	s.deliverMediaJSON(w, x, out, body)
	s.finishMedia(x, out, out.status)
}

// videoContent streams the owned job's content variant through the spool.
func (s *Server) videoContent(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.mediaBegin(w, r)
	if done {
		return
	}
	defer s.release(r.Context())
	x.family = openai.FamilyVideoContent
	x.mode = "unary"
	ctx, complete, admissionError := s.admitVideoRequest(r.Context(), x, authority)
	if admissionError != nil {
		s.mediaFail(x, w, admissionError)
		return
	}
	defer complete()
	r = r.WithContext(ctx)
	record, e := s.ownedJob(r.Context(), authority, r.PathValue("video_id"), media.OpVideoContent)
	if e != nil {
		s.mediaFail(x, w, e)
		return
	}
	variant, failure := media.ValidateVideoContentQuery(r.URL.Query())
	if failure != nil {
		s.mediaFail(x, w, mediaError(failure))
		return
	}
	if record.UpstreamJobID == nil || !media.ValidUpstreamJobID(*record.UpstreamJobID) {
		s.mediaFail(x, w, serverError(http.StatusServiceUnavailable, "media_job_upstream_id_unavailable", "The video job's upstream identity is unavailable."))
		return
	}
	x.route = &runtime.Route{Slug: record.RouteSlug}
	call, failure := media.Encode(&media.Request{Op: media.OpVideoContent, JobID: *record.UpstreamJobID, Variant: variant, Route: record.RouteSlug}, "openai", record.UpstreamModel)
	if failure != nil {
		s.mediaFail(x, w, mediaError(failure))
		return
	}
	result, transportFailure, fact := s.videoJobCall(r.Context(), x, record, call, &media.Request{Op: media.OpVideoContent, Route: record.RouteSlug})
	x.dispatched = transportFailure == nil || transportFailure.dispatched
	x.facts = append(x.facts, fact)
	if transportFailure != nil {
		s.mediaFailOutcome(x, w, transportFailure)
		return
	}
	if result.Artifact == nil {
		s.mediaFailOutcome(x, w, &attemptFailure{class: classProtocol})
		return
	}
	out := &mediaOutcome{result: result, committed: true, status: http.StatusOK}
	s.streamArtifact(w, x, out)
	s.finishMedia(x, out, out.status)
}

// videoDelete persists delete intent, then issues and confirms the upstream
// delete before the local tombstone is finalized.
func (s *Server) videoDelete(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.mediaBegin(w, r)
	if done {
		return
	}
	defer s.release(r.Context())
	x.family = openai.FamilyVideoDelete
	x.mode = "unary"
	ctx, complete, admissionError := s.admitVideoRequest(r.Context(), x, authority)
	if admissionError != nil {
		s.mediaFail(x, w, admissionError)
		return
	}
	defer complete()
	r = r.WithContext(ctx)
	loaded, e := s.ownedJob(r.Context(), authority, r.PathValue("video_id"), media.OpVideoDelete)
	if e != nil {
		s.mediaFail(x, w, e)
		return
	}
	record := *loaded
	if record.Lifecycle == media.LifecycleDeleted {
		body, _ := media.EncodeVideoDeleteResponse(&media.VideoDeleteResult{ID: record.ID, Deleted: true}, record.ID)
		out := &mediaOutcome{committed: true, status: http.StatusOK}
		x.dispatched = true // Serving an existing tombstone consumes one key request.
		s.deliverMediaJSON(w, x, out, body)
		s.finishMedia(x, out, out.status)
		return
	}
	if record.UpstreamJobID == nil || !media.ValidUpstreamJobID(*record.UpstreamJobID) {
		s.mediaFail(x, w, serverError(http.StatusServiceUnavailable, "media_job_upstream_id_unavailable", "The video job's upstream identity is unavailable."))
		return
	}
	x.route = &runtime.Route{Slug: record.RouteSlug}
	call, failure := media.Encode(&media.Request{Op: media.OpVideoDelete, JobID: *record.UpstreamJobID, Route: record.RouteSlug}, "openai", record.UpstreamModel)
	if failure != nil {
		s.mediaFail(x, w, mediaError(failure))
		return
	}
	result, transportFailure, fact := s.videoJobCall(r.Context(), x, &record, call, &media.Request{Op: media.OpVideoDelete, Route: record.RouteSlug})
	x.dispatched = transportFailure == nil || transportFailure.dispatched
	x.facts = append(x.facts, fact)
	deleted := false
	if transportFailure != nil {
		// The delete-missing-is-success rule: an upstream 404 confirms the
		// object is already gone.
		if transportFailure.status != 404 {
			s.mediaFailOutcome(x, w, transportFailure)
			return
		}
		deleted = true
	} else {
		if result.Deleted == nil {
			s.mediaFailOutcome(x, w, &attemptFailure{class: classProtocol})
			return
		}
		deleted = result.Deleted.Deleted
	}
	if !deleted {
		s.mediaFailOutcome(x, w, &attemptFailure{class: classUpstreamClient, status: http.StatusBadGateway})
		return
	}
	finalized, err := s.Media.Jobs.FinalizeDeletion(r.Context(), record.ID)
	if err != nil {
		s.mediaFail(x, w, mediaError(media.JobHTTPError(err)))
		return
	}
	if !finalized {
		s.Media.Jobs.RecordGap()
		s.mediaFailOutcome(x, w, &attemptFailure{class: classConnect})
		return
	}
	local := ""
	var object *string
	var extra map[string]any
	if result != nil && result.Deleted != nil {
		object = result.Deleted.Object
		extra = result.Deleted.Extra
	}
	_ = object
	body, failure := media.EncodeVideoDeleteResponse(&media.VideoDeleteResult{ID: local, Deleted: true, Extra: extra}, record.ID)
	if failure != nil {
		s.mediaFailOutcome(x, w, &attemptFailure{class: classProtocol})
		return
	}
	out := &mediaOutcome{result: result, committed: true, status: http.StatusOK}
	s.deliverMediaJSON(w, x, out, body)
	s.finishMedia(x, out, out.status)
}

// mediaFailOutcome records a post-dispatch media failure and writes it.
func (s *Server) mediaFailOutcome(x *execution, w http.ResponseWriter, failure *attemptFailure) {
	e := failure.toError()
	s.finishMedia(x, &mediaOutcome{err: e, committed: failure.committed}, e.Status)
	writeSurfaceError(w, e, "openai")
}

// mediaJobResult renders a stored record as the client-facing video object.
func mediaJobResult(record *media.JobRecord) media.VideoJobResult {
	status := "queued"
	switch record.State {
	case media.StateRunning:
		status = "in_progress"
	case media.StateSucceeded:
		status = "completed"
	case media.StateFailed:
		status = "failed"
	case media.StateCancelled:
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
