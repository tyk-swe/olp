package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/mediacontract"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
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
	x.family = wire.Format(request.Op())
	x.media = request
	x.estimate = mediaMultipartTokens
	defer s.release(r.Context())
	defer s.cleanupUploads(request)

	jobID, err := uuid.NewV7()
	if err != nil {
		form.Cleanup()
		s.mediaFail(x, w, jobError(err))
		return
	}
	localJobID := jobID.String()
	x.affinity = []byte(localJobID)
	if e := s.prepareMedia(r.Context(), x, authority); e != nil {
		form.Cleanup()
		s.mediaFail(x, w, e)
		return
	}
	if e := s.enforceMediaInput(x); e != nil {
		form.Cleanup()
		s.mediaFail(x, w, e)
		return
	}
	form.Disarm()
	// The reservation pins exactly one provider target; create runs on that
	// target alone because a second provider would mint a second job.
	attempt := x.attempts[0]
	provider, ok := x.request.release.Snapshot().FindProvider(attempt.ProviderID)
	if !ok {
		s.mediaFail(x, w, serverError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider is currently able to serve `"+x.route.Slug+"`."))
		return
	}
	if e := x.checkCallerTarget(&provider, attempt.UpstreamModel); e != nil {
		s.mediaFail(x, w, e)
		return
	}
	slots := s.slots(x, attempt, &provider)
	if len(slots) == 0 {
		s.mediaFail(x, w, serverError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider credential is currently able to serve `"+x.route.Slug+"`."))
		return
	}

	overall := time.Duration(x.route.OverallTimeout) * time.Millisecond
	ctx, cancel := x.routeContext(r.Context())
	defer cancel()
	var e *Error
	admitted := x.admissionAuthority(authority)
	if x.lease, e = s.deps.Admission.reserveKeyCosted(ctx, &admitted, x.clientSurface(), x.estimate, overall, x.attemptCostReservation(authority, attempt), x.limitRoute()); e != nil {
		s.mediaFail(x, w, e)
		return
	}
	defer s.settleAdmission(ctx, x)

	deadline, _ := ctx.Deadline()
	hold, slot, e := s.admitVideoSlot(ctx, x, attempt, &provider, slots, deadline)
	if hold == nil {
		s.mediaFail(x, w, e)
		return
	}
	defer func() { hold.settle(ctx, x.dispatched, x.settledTokens()) }()

	reserved, jobErr := s.deps.Media.Store.Reserve(ctx, jobs.Reservation{
		ID:                  localJobID,
		StrictContract:      x.strict(),
		RuntimeGenerationID: x.request.release.Snapshot().Generation().ID,
		ProviderRevisionID:  attempt.ProviderRevisionID,
		APIKeyID:            authority.ID,
		ProviderID:          attempt.ProviderID,
		UpstreamModel:       attempt.UpstreamModel,
		RouteSlug:           x.route.Slug,
		Surface:             "openai",
		CredentialVersionID: slot.CredentialID,
		SlotID:              slot.ID,
		StaleAfter:          deadline.Add(jobs.LeaseSlack),
	})
	if jobErr != nil {
		s.releaseHold(ctx, hold)
		s.mediaFail(x, w, jobError(jobErr))
		return
	}

	// The accepted upstream create must outlive a client disconnect: run the
	// dispatch on a detached context bounded by the route deadline.
	dispatchCtx, dispatchCancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer dispatchCancel()
	fact := s.newFact(x, attempt, slot, len(x.facts)+1)
	fact.Budgets = hold.budgets
	fact, result, dispatchFailure := s.mediaAttempt(dispatchCtx, w, x, attempt, &provider, slot, fact)
	x.dispatched = dispatchFailure == nil || dispatchFailure.dispatched
	if x.dispatched {
		x.spendCaps(hold.budgets, attempt)
	}
	x.facts = append(x.facts, fact)
	s.health.record(provider.ID, fact, hold.probed)
	// A reply that lands just before the route deadline must still bind or
	// retire its job: persistence gets its own bound, independent of the
	// spent dispatch deadline.
	commitCtx, stopCommit := resourceCommitContext(ctx)
	defer stopCommit()
	if dispatchFailure != nil {
		s.deps.Media.Jobs.Abandon(commitCtx, reserved.ID, dispatchFailure.class == classAmbiguous)
		out := &mediaOutcome{err: dispatchFailure.toError(), committed: dispatchFailure.committed, cancelled: dispatchFailure.class == classCancelled}
		s.finishMedia(x, out, out.err.Status)
		writeSurfaceError(w, out.err, "openai")
		return
	}
	out := s.attachCreated(commitCtx, x, reserved, result)
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
func (s *Server) admitVideoSlot(ctx context.Context, x *call, attempt routing.Attempt, provider *routing.Provider, slots []routing.Slot, deadline time.Time) (*dispatchHold, routing.Slot, *Error) {
	used := 0
	unmeterable := false
	var last *attemptFailure
	for _, slot := range slots {
		if used >= x.budget || ctx.Err() != nil {
			break
		}
		gate := s.gateAttempt(ctx, x, attempt, provider, &slot, x.estimate, deadline)
		switch gate.verdict {
		case gateExpired:
			return nil, routing.Slot{}, (&attemptFailure{class: classTimeout}).toError()
		case gateDenied:
			// The circuit refused the probe; siblings share the endpoint.
			return nil, routing.Slot{}, s.videoAdmissionError(ctx, x, last, unmeterable)
		case gateRejected:
			used++
			x.facts = append(x.facts, s.rejectedFact(x, attempt, slot, used, gate.rejection))
			last = gate.rejection
			if gate.rejection.quota == quotaConnection || gate.rejection.quota == quotaRoute {
				return nil, routing.Slot{}, s.videoAdmissionError(ctx, x, last, unmeterable)
			}
		case gateUnmeterable:
			unmeterable = true
			if gate.quota == quotaConnection || gate.quota == quotaRoute {
				return nil, routing.Slot{}, s.videoAdmissionError(ctx, x, last, unmeterable)
			}
		case gateAdmitted:
			return gate.hold, slot, nil
		}
	}
	return nil, routing.Slot{}, s.videoAdmissionError(ctx, x, last, unmeterable)
}

// videoAdmissionError renders the terminal error when no candidate slot could
// be admitted, matching the precedence the media attempt loop uses.
func (s *Server) videoAdmissionError(ctx context.Context, x *call, last *attemptFailure, unmeterable bool) *Error {
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

func (s *Server) attachCreated(ctx context.Context, x *call, reserved jobs.Record, result *transport.Result) *mediaOutcome {
	out := &mediaOutcome{result: result, committed: true, status: http.StatusCreated, localJob: reserved.ID}
	err := s.deps.Media.Jobs.Attach(ctx, reserved, mediaVideo(result), &jobs.Caller{Secret: x.callerSecret, Sensitive: &x.sensitive})
	rejected, isRejected := errors.AsType[*jobs.Rejected](err)
	switch {
	case isRejected:
		out.err = serverError(http.StatusBadGateway, "provider_protocol_error", rejected.Message)
	case err != nil:
		out.err = serverError(http.StatusServiceUnavailable, "media_job_create_reconciliation_pending", "The video creation is being reconciled; check the job status shortly.")
	}
	return out
}

func jobError(err error) *Error {
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

// ownedJob loads a media job the caller is allowed to see, enforcing
// ownership, lifecycle visibility, and the route permission.
func (s *Server) ownedJob(ctx context.Context, authority keyauthority.Authority, videoID string, op media.Operation) (*jobs.Record, *Error) {
	id, err := uuid.Parse(videoID)
	if err != nil {
		return nil, notFoundError("video_not_found", "The video job does not exist.")
	}
	record, err := s.deps.Media.Store.Get(ctx, id.String())
	if err != nil {
		return nil, jobError(err)
	}
	if record.APIKeyID != authority.ID {
		return nil, notFoundError("video_not_found", "The video job does not exist.")
	}
	if record.Lifecycle == jobs.Deleted && op != media.OpVideoDelete {
		return nil, notFoundError("video_not_found", "The video job does not exist.")
	}
	switch record.Lifecycle {
	case jobs.Active, jobs.DeletePending, jobs.Deleted:
	default:
		return nil, serverError(http.StatusServiceUnavailable, "media_job_reconciliation_pending", "The video job is being reconciled; retry shortly.")
	}
	projectID, current, e := s.jobRoute(ctx, record.RouteSlug)
	if e != nil {
		return nil, e
	}
	// A job whose route the key may no longer use answers like every other
	// retained resource the key cannot reach.
	if !authority.Allows("inference", record.RouteSlug, projectID, s.now()) {
		return nil, notFoundError("video_not_found", "The video job does not exist.")
	}
	use := retainedRetrieval
	if op == media.OpVideoDelete {
		use = retainedHousekeeping
	}
	if e := retainedContract(record.StrictContract, current, use); e != nil {
		return nil, e
	}
	return &record, nil
}

func (s *Server) jobRoute(ctx context.Context, slug string) (*string, routing.RouteFidelity, *Error) {
	projectID, current, err := s.deps.Media.Store.RouteAuthority(ctx, slug)
	if err != nil {
		return nil, routing.RouteFidelity{}, serverError(http.StatusServiceUnavailable, "route_unavailable", "The video job's route could not be read.")
	}
	return projectID, current, nil
}

func strictVideoResourceContext(x *call, record *jobs.Record, allowVariant bool) *Error {
	if !record.StrictContract {
		return nil
	}
	if e := strictMediaHeaders(x); e != nil {
		return e
	}
	if x.semanticQueryInvalid || !allowVariant && len(x.semanticQuery) != 0 {
		return invalidRequest("target_capability", "This strict video resource does not qualify caller query settings.", nil)
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

// admitVideoRequest applies key budgets once, before any upstream call or
// mutation. A call on a retained job admits once the job names its route, so
// the route's limits and caller-paid policy apply.
func (s *Server) admitVideoRequest(parent context.Context, x *call, authority keyauthority.Authority) (context.Context, func(), *Error) {
	ttl := maxStreamDuration + jobs.LeaseSlack
	x.budgetGroupID = authority.BudgetGroupID
	ctx, cancel := context.WithTimeout(parent, ttl)
	var e *Error
	// A job call carries no prompt, and reserves what any other call on a resource
	// does: a key with a token limit refuses a reservation of nothing.
	x.lease, e = s.deps.Admission.reserveKey(ctx, x.admissionAuthority(authority), x.clientSurface(), resourceEstimate, ttl, x.limitRoute())
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
// videoJobCall makes one call on an existing job, encoded in the wire of the
// vendor the job's pinned provider revision names.
func (s *Server) videoJobCall(ctx context.Context, x *call, record *jobs.Record, job media.JobCall) (*transport.Result, *attemptFailure, AttemptFact) {
	fact := AttemptFact{
		TargetID:           record.ID,
		ProviderID:         record.ProviderID,
		ProviderRevisionID: record.ProviderRevisionID,
		UpstreamModel:      record.UpstreamModel,
		ModelFamily:        string(estimate.FamilyOf(record.UpstreamModel)),
		Mode:               "unary",
		StartedAt:          s.now(),
	}
	if record.CredentialVersionID != nil {
		fact.CredentialID = *record.CredentialVersionID
	}
	target, err := s.deps.Media.Jobs.Target(ctx, *record)
	if err != nil {
		fact.Class = classConnect
		fact.Duration = s.now().Sub(fact.StartedAt)
		fact.recordEvidence(false)
		return nil, &attemptFailure{class: classConnect}, fact
	}
	// Connection and credential authority stay pinned; quota changes in the
	// current release still apply to subsequent requests for the retained job.
	provider, slot := target.Provider, target.Slot
	if provider.CredentialSource == "caller" {
		fact.CredentialSource, fact.CredentialID, fact.CredentialVersion = "caller", "", 0
		fact.BudgetExempt = x.callerCostExempt()
	}

	if e := x.checkCallerTarget(&provider, record.UpstreamModel); e != nil {
		fact.Class = classCredential
		fact.Duration = s.now().Sub(fact.StartedAt)
		fact.recordEvidence(false)
		return nil, &attemptFailure{class: classCredential}, fact
	}
	// Each job call redacts with the secrets of its own pinned provider.
	var sensitive egress.Sensitive
	if provider.CredentialSource == "caller" {
		secret, err := x.bindCallerSecret(provider.ID, record.UpstreamModel)
		if err != nil {
			fact.Class = classCredential
			fact.Duration = s.now().Sub(fact.StartedAt)
			fact.recordEvidence(false)
			return nil, &attemptFailure{class: classCredential}, fact
		}
		sensitive.Add(string(secret))
		target.Secret, target.CallerCredential = secret, true
	}

	fact.VendorID = provider.VendorID
	call, encodeFailure := media.EncodeJob(job, target.Config)
	if encodeFailure != nil {
		fact.Class = classProtocol
		fact.Duration = s.now().Sub(fact.StartedAt)
		fact.recordEvidence(false)
		return nil, &attemptFailure{class: classProtocol}, fact
	}
	var strictTemplate *mediacontract.Template
	if record.StrictContract {
		var err error
		strictTemplate, err = mediacontract.Compile(mediacontract.Config{
			Provider: target.Config, ProviderID: record.ProviderID,
			RevisionID: record.ProviderRevisionID, Model: record.UpstreamModel,
			Operation: string(job.Operation()),
		})
		if err != nil {
			fact.Class = classProtocol
			fact.Duration = s.now().Sub(fact.StartedAt)
			fact.recordEvidence(false)
			return nil, &attemptFailure{class: classProtocol}, fact
		}
	}
	if current, ok := x.request.release.Snapshot().FindProvider(provider.ID); ok {
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
	reservation, rejection, skip := s.deps.Admission.reserveTarget(ctx, &provider, &slot, 0, target.Timeout+jobs.LeaseSlack, "")
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
	if _, deleting := job.(media.VideoDelete); deleting {
		if _, err := s.deps.Media.Store.BeginDeletion(ctx, record.ID, jobs.Unfenced); err != nil {
			fact.Class = classConnect
			fact.Duration = s.now().Sub(fact.StartedAt)
			fact.recordEvidence(false)
			return nil, &attemptFailure{class: classConnect}, fact
		}
	}
	attemptCtx, atr := x.request.trace.Attempt(ctx, string(target.Config.Kind), record.ProviderRevisionID, record.UpstreamModel)
	headers := http.Header{}
	atr.InjectUpstream(headers)
	callCtx, cancel := context.WithTimeout(attemptCtx, target.Timeout+jobs.LeaseSlack)
	defer cancel()
	target.Sensitive = &sensitive
	result, failure := s.deps.Media.Transport.Do(callCtx, target.Target, call, transport.Options{Strict: strictTemplate != nil, Headers: headers})
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
			acceptance: upstream.Evidence{Reached: failure.Dispatched, Status: failure.Status}.Acceptance(),
		}
		fact.recordEvidence(f.billingUncertain())
		s.health.record(record.ProviderID, fact, false)
		atr.Finish(fact.Class, fact.Status)
		return nil, f, fact
	}
	if strictTemplate != nil {
		var contractErr error
		switch job.(type) {
		case media.VideoGet, media.VideoDelete:
			if !result.Source.Valid() || record.UpstreamJobID == nil {
				contractErr = errors.New("native video source is unavailable")
			} else {
				_, contractErr = strictTemplate.JSONResult(strictTemplate.Descriptor("unary"), result.Source)
				if contractErr == nil {
					id, _ := result.Source.Root().Lookup("id")
					actual, _ := id.Text()
					if actual != *record.UpstreamJobID {
						contractErr = errors.New("native video identity changed")
					}
				}
			}
		case media.VideoContent:
			if artifact := mediaArtifact(result.Payload); artifact == nil {
				contractErr = errors.New("native video content is unavailable")
			} else {
				contractErr = strictBlob(strictTemplate.Descriptor("unary"), artifact)
			}
		}
		if contractErr != nil {
			if artifact := mediaArtifact(result.Payload); artifact != nil {
				if err := s.deps.Staging.Remove(artifact.Handle); err != nil {
					s.log.Warn("staged video content cleanup failed", "error", err)
				}
			}
			fact.Class = classProtocol
			fact.Duration = s.now().Sub(fact.StartedAt)
			fact.recordEvidence(true)
			atr.Finish(fact.Class, result.Status)
			return nil, &attemptFailure{class: classProtocol, status: result.Status, dispatched: true}, fact
		}
	}
	fact.Class = "success"
	fact.Status = result.Status
	fb := result.FirstByte
	fact.FirstByte = &fb
	fact.Usage = mediaUsage(nil, result)
	fact.Committed = true
	fact.recordEvidence(false)
	s.health.record(record.ProviderID, fact, false)
	if u := fact.Usage; u != nil {
		atr.RecordUsage(&u.InputTokens, &u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
	}
	atr.Finish(fact.Class, fact.Status)
	return result, nil, fact
}

// noteJobAttempt records the one call a job read, content download or delete
// made, which is the attempt that serves the response.
func (x *call) noteJobAttempt(fact AttemptFact) {
	x.facts = append(x.facts, fact)
	x.attemptCount, x.attemptVendor = len(x.facts), fact.VendorID
}

// notePolls states the polls a list made, once they have all been recorded. They
// are its attempts, and it names a provider only when one served them all: a list
// that reached several has no single vendor to name, and one that polled nothing
// made no attempt.
func (x *call) notePolls() {
	x.attemptCount, x.attemptVendor = len(x.facts), ""
	if len(x.facts) == 0 {
		return
	}
	x.attemptVendor = x.facts[0].VendorID
	for _, fact := range x.facts[1:] {
		if fact.VendorID != x.attemptVendor {
			x.attemptVendor = ""
			return
		}
	}
}

// videoList pages the caller's jobs and refreshes non-terminal records.
func (s *Server) videoList(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.mediaBegin(w, r)
	if done {
		return
	}
	defer s.release(r.Context())
	x.family = wire.VideoList
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
		s.mediaFail(x, w, jobError(err))
		return
	}
	listing.After = after
	page, err := s.deps.Media.Store.List(r.Context(), listing)
	if err != nil {
		s.mediaFail(x, w, jobError(err))
		return
	}
	refreshed := make([]jobs.Record, 0, len(page.Items))
	var refreshError *Error
	// This request holds one process admission permit. Poll serially so a
	// caller-controlled page cannot multiply that permit into 100 requests.
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
		s.mediaFail(x, w, refreshError)
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
func (s *Server) refreshListRecord(ctx context.Context, x *call, record jobs.Record) (jobs.Record, *AttemptFact, bool, *Error) {
	upstream, ok := record.Upstream()
	if !record.State.Polling() || !ok {
		return record, nil, false, nil
	}
	// A poll retrieves the job, so a job its route no longer serves is listed
	// from its stored state, like retained files and batches.
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

// videoGet refreshes and renders one owned job.
func (s *Server) videoGet(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.mediaBegin(w, r)
	if done {
		return
	}
	defer s.release(r.Context())
	x.family = wire.VideoGet
	x.mode = "unary"
	record, e := s.ownedJob(r.Context(), authority, r.PathValue("video_id"), media.OpVideoGet)
	if e != nil {
		s.mediaFail(x, w, e)
		return
	}
	if e = strictVideoResourceContext(x, record, false); e != nil {
		s.mediaFail(x, w, e)
		return
	}
	x.route = videoJobRoute(record)
	ctx, complete, admissionError := s.admitVideoRequest(r.Context(), x, authority)
	if admissionError != nil {
		s.mediaFail(x, w, admissionError)
		return
	}
	defer complete()
	r = r.WithContext(ctx)
	upstream, ok := record.Upstream()
	if !ok {
		s.mediaFail(x, w, serverError(http.StatusServiceUnavailable, "media_job_upstream_id_unavailable", "The video job's upstream identity is unavailable."))
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

// videoContent streams the owned job's content variant through the spool.
func (s *Server) videoContent(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.mediaBegin(w, r)
	if done {
		return
	}
	defer s.release(r.Context())
	x.family = wire.VideoContent
	x.mode = "unary"
	record, e := s.ownedJob(r.Context(), authority, r.PathValue("video_id"), media.OpVideoContent)
	if e != nil {
		s.mediaFail(x, w, e)
		return
	}
	if e = strictVideoResourceContext(x, record, true); e != nil {
		s.mediaFail(x, w, e)
		return
	}
	x.route = videoJobRoute(record)
	ctx, complete, admissionError := s.admitVideoRequest(r.Context(), x, authority)
	if admissionError != nil {
		s.mediaFail(x, w, admissionError)
		return
	}
	defer complete()
	r = r.WithContext(ctx)
	variant, failure := media.ValidateVideoContentQuery(r.URL.Query())
	if failure != nil {
		s.mediaFail(x, w, mediaError(failure))
		return
	}
	upstream, ok := record.Upstream()
	if !ok {
		s.mediaFail(x, w, serverError(http.StatusServiceUnavailable, "media_job_upstream_id_unavailable", "The video job's upstream identity is unavailable."))
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

// videoDelete persists delete intent, then issues and confirms the upstream
// delete before the local tombstone is finalized.
func (s *Server) videoDelete(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.mediaBegin(w, r)
	if done {
		return
	}
	defer s.release(r.Context())
	x.family = wire.VideoDelete
	x.mode = "unary"
	loaded, e := s.ownedJob(r.Context(), authority, r.PathValue("video_id"), media.OpVideoDelete)
	if e != nil {
		s.mediaFail(x, w, e)
		return
	}
	if e = strictVideoResourceContext(x, loaded, false); e != nil {
		s.mediaFail(x, w, e)
		return
	}
	x.route = videoJobRoute(loaded)
	ctx, complete, admissionError := s.admitVideoRequest(r.Context(), x, authority)
	if admissionError != nil {
		s.mediaFail(x, w, admissionError)
		return
	}
	defer complete()
	r = r.WithContext(ctx)
	record := *loaded
	if record.Lifecycle == jobs.Deleted {
		if record.StrictContract {
			s.mediaFail(x, w, notFoundError("video_not_found", "The video job does not exist."))
			return
		}
		body, _ := media.EncodeVideoDeleteResponse(&media.VideoDeleteResult{ID: record.ID, Deleted: true}, record.ID)
		out := &mediaOutcome{committed: true, status: http.StatusOK}
		x.dispatched = true // Serving an existing tombstone consumes one key request.
		s.deliverMediaJSON(w, x, out, body)
		s.finishMedia(x, out, out.status)
		return
	}
	upstream, ok := record.Upstream()
	if !ok {
		s.mediaFail(x, w, serverError(http.StatusServiceUnavailable, "media_job_upstream_id_unavailable", "The video job's upstream identity is unavailable."))
		return
	}
	result, transportFailure, fact := s.videoJobCall(r.Context(), x, &record, media.VideoDelete{ID: upstream})
	x.dispatched = transportFailure == nil || transportFailure.dispatched
	x.noteJobAttempt(fact)
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
		s.mediaFail(x, w, jobError(err))
		return
	}
	if finalized.Lifecycle != jobs.Deleted {
		s.deps.Media.Jobs.RecordGap()
		s.mediaFailOutcome(x, w, &attemptFailure{class: classConnect})
		return
	}
	if record.StrictContract && result == nil {
		// An upstream 404 proves the resource is gone, but did not provide a
		// native deletion receipt. Surface that 404 after finalizing the local
		// tombstone rather than inventing a successful native document.
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

// mediaFailOutcome records a post-dispatch media failure and writes it.
func (s *Server) mediaFailOutcome(x *call, w http.ResponseWriter, failure *attemptFailure) {
	e := failure.toError()
	s.finishMedia(x, &mediaOutcome{err: e, committed: failure.committed}, e.Status)
	writeSurfaceError(w, e, "openai")
}

// mediaJobResult renders a stored record as the client-facing video object.
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
