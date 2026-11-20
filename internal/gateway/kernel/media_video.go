package kernel

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
)

type videoTarget struct {
	attempt  routing.Attempt
	provider *routing.Provider
	slots    []routing.Slot
}

func (s *Server) CreateVideoJob(w http.ResponseWriter, r *http.Request, x *Call, authority keyauthority.Authority, form *staging.Form, request *media.Request) {
	x.family = wire.Format(request.Op())
	x.media = request
	x.estimate = mediaMultipartTokens
	defer s.cleanupUploads(request)

	jobID, err := uuid.NewV7()
	if err != nil {
		form.Cleanup()
		s.Refuse(x, w, JobError(err))
		return
	}
	x.affinity = []byte(jobID.String())
	if e := s.prepareMedia(r.Context(), x, authority); e != nil {
		form.Cleanup()
		s.Refuse(x, w, e)
		return
	}
	form.Disarm()
	target, e := s.videoTarget(x)
	if e != nil {
		s.Refuse(x, w, e)
		return
	}

	overall := time.Duration(x.Route.OverallTimeout) * time.Millisecond
	ctx, cancel := x.routeContext(r.Context())
	defer cancel()
	admitted := x.AdmissionAuthority(authority)
	if x.lease, e = s.Deps.Admission.ReserveKeyCosted(ctx, &admitted, x.clientSurface(), x.estimate, overall, x.attemptCostReservation(authority, target.attempt), x.limitRoute()); e != nil {
		s.Refuse(x, w, e)
		return
	}
	defer s.settleAdmission(ctx, x)

	deadline, _ := ctx.Deadline()
	hold, slot, e := s.admitVideoSlot(ctx, x, target.attempt, target.provider, target.slots, deadline)
	if hold == nil {
		s.Refuse(x, w, e)
		return
	}
	defer func() { hold.settle(ctx, x.dispatched, x.settledTokens()) }()

	reserved, jobErr := s.Deps.Media.Store.Reserve(ctx, videoReservation(x, authority, target.attempt, slot, jobID.String(), deadline))
	if jobErr != nil {
		s.ReleaseHold(ctx, hold)
		s.Refuse(x, w, JobError(jobErr))
		return
	}
	s.submitVideo(ctx, w, x, target, slot, hold, reserved, deadline)
}

func (s *Server) ReadVideoCreate(w http.ResponseWriter, r *http.Request, authority keyauthority.Authority) (*staging.Form, *media.Request, *fault.Error) {
	form, parseError := s.ParseMediaForm(w, r, authority.ID, media.DefaultVideoReferenceLimit, 1)
	if parseError != nil {
		return nil, nil, parseError
	}
	request, failure := media.DecodeVideoCreate(form)
	if failure != nil {
		form.Cleanup()
		return nil, nil, MediaError(failure)
	}
	return form, request, nil
}

func (s *Server) videoTarget(x *Call) (videoTarget, *fault.Error) {
	attempt := x.attempts[0]
	provider, ok := x.request.release.Snapshot().FindProvider(attempt.ProviderID)
	if !ok {
		return videoTarget{}, fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider is currently able to serve `"+x.Route.Slug+"`.")
	}
	if e := x.checkCallerTarget(&provider, attempt.UpstreamModel); e != nil {
		return videoTarget{}, e
	}
	slots := s.slots(x, attempt, &provider)
	if len(slots) == 0 {
		return videoTarget{}, fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider credential is currently able to serve `"+x.Route.Slug+"`.")
	}
	return videoTarget{attempt: attempt, provider: &provider, slots: slots}, nil
}

func videoReservation(x *Call, authority keyauthority.Authority, attempt routing.Attempt, slot routing.Slot, jobID string, deadline time.Time) jobs.Reservation {
	return jobs.Reservation{
		ID:                  jobID,
		StrictContract:      x.Strict(),
		RuntimeGenerationID: x.request.release.Snapshot().Generation().ID,
		ProviderRevisionID:  attempt.ProviderRevisionID,
		APIKeyID:            authority.ID,
		ProviderID:          attempt.ProviderID,
		UpstreamModel:       attempt.UpstreamModel,
		RouteSlug:           x.Route.Slug,
		Surface:             "openai",
		CredentialVersionID: slot.CredentialID,
		SlotID:              slot.ID,
		StaleAfter:          deadline.Add(jobs.LeaseSlack),
	}
}

func (s *Server) submitVideo(ctx context.Context, w http.ResponseWriter, x *Call, target videoTarget, slot routing.Slot, hold *dispatchHold, reserved jobs.Record, deadline time.Time) {
	dispatchCtx, dispatchCancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer dispatchCancel()
	planned := s.newFact(x, target.attempt, slot, len(x.facts)+1)
	planned.Budgets = hold.budgets
	fact, result, dispatchFailure := s.mediaAttempt(dispatchCtx, w, x, target.attempt, target.provider, slot, planned)
	x.dispatched = dispatchFailure == nil || dispatchFailure.dispatched
	if x.dispatched {
		x.spendCaps(hold.budgets, target.attempt)
	}
	x.facts = append(x.facts, fact)
	s.health.Record(target.provider.ID, fact, hold.probed)
	commitCtx, stopCommit := ResourceCommitContext(ctx)
	defer stopCommit()
	if dispatchFailure != nil {
		s.Deps.Media.Jobs.Abandon(commitCtx, reserved.ID, dispatchFailure.class == ClassAmbiguous)
		out := &MediaOutcome{err: dispatchFailure.Fault(), committed: dispatchFailure.committed, cancelled: dispatchFailure.class == ClassCancelled}
		s.finishMedia(x, out, out.err.Status)
		fault.WriteSurfaceError(w, out.err, "openai")
		return
	}
	out := s.attachCreated(commitCtx, x, reserved, result)
	if out.err != nil {
		s.finishMedia(x, out, out.err.Status)
		fault.WriteSurfaceError(w, out.err, "openai")
		return
	}
	s.writeMediaResult(w, x, out)
	s.finishMedia(x, out, out.status)
}

func (s *Server) admitVideoSlot(ctx context.Context, x *Call, attempt routing.Attempt, provider *routing.Provider, slots []routing.Slot, deadline time.Time) (*dispatchHold, routing.Slot, *fault.Error) {
	used := 0
	unmeterable := false
	var last *AttemptFailure
	for _, slot := range slots {
		if used >= x.budget || ctx.Err() != nil {
			break
		}
		gate := s.gateAttempt(ctx, x, attempt, provider, &slot, x.estimate, deadline)
		switch gate.verdict {
		case gateExpired:
			return nil, routing.Slot{}, (&AttemptFailure{class: ClassTimeout}).Fault()
		case gateDenied:
			return nil, routing.Slot{}, s.videoAdmissionError(ctx, x, last, unmeterable)
		case gateRejected:
			used++
			x.facts = append(x.facts, s.rejectedFact(x, attempt, slot, used, gate.rejection))
			last = gate.rejection
			if gate.rejection.quota == quota.Connection || gate.rejection.quota == quota.Route {
				return nil, routing.Slot{}, s.videoAdmissionError(ctx, x, last, unmeterable)
			}
		case gateUnmeterable:
			unmeterable = true
			if gate.quota == quota.Connection || gate.quota == quota.Route {
				return nil, routing.Slot{}, s.videoAdmissionError(ctx, x, last, unmeterable)
			}
		case gateAdmitted:
			return gate.hold, slot, nil
		}
	}
	return nil, routing.Slot{}, s.videoAdmissionError(ctx, x, last, unmeterable)
}

func (s *Server) videoAdmissionError(ctx context.Context, x *Call, last *AttemptFailure, unmeterable bool) *fault.Error {
	switch {
	case ctx.Err() != nil && errors.Is(context.Cause(ctx), context.DeadlineExceeded):
		return (&AttemptFailure{class: ClassTimeout}).Fault()
	case ctx.Err() != nil:
		return (&AttemptFailure{class: ClassCancelled}).Fault()
	case last != nil:
		return last.Fault()
	case unmeterable:
		return quota.LimitsUnavailable()
	}
	return fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider credential is currently able to serve `"+x.Route.Slug+"`.")
}

func (s *Server) attachCreated(ctx context.Context, x *Call, reserved jobs.Record, result *transport.Result) *MediaOutcome {
	out := &MediaOutcome{result: result, committed: true, status: http.StatusCreated, localJob: reserved.ID}
	err := s.Deps.Media.Jobs.Attach(ctx, reserved, MediaVideo(result), &jobs.Caller{Secret: x.callerSecret, Sensitive: &x.sensitive})
	rejected, isRejected := errors.AsType[*jobs.Rejected](err)
	switch {
	case isRejected:
		out.err = fault.ServerError(http.StatusBadGateway, "provider_protocol_error", rejected.Message)
	case err != nil:
		out.err = fault.ServerError(http.StatusServiceUnavailable, "media_job_create_reconciliation_pending", "The video creation is being reconciled; check the job status shortly.")
	}
	return out
}

func JobError(err error) *fault.Error {
	failure, ok := errors.AsType[*jobs.Error](err)
	if !ok {
		return MediaError(media.Fail(http.StatusServiceUnavailable, "media_job_unavailable", "The media job store is unavailable."))
	}
	switch failure.Kind {
	case jobs.KindNotFound:
		return MediaError(media.Fail(http.StatusNotFound, "media_job_not_found", "The media job does not exist."))
	case jobs.KindChanged:
		return MediaError(media.Fail(http.StatusConflict, "media_job_changed", "The media job changed; refresh and retry."))
	case jobs.KindConflict:
		return MediaError(media.Fail(http.StatusConflict, "media_job_upstream_conflict", "The upstream media job identity conflicts with stored metadata."))
	case jobs.KindInvalid:
		return MediaError(media.Fail(http.StatusBadRequest, "invalid_request", failure.Message))
	}
	return MediaError(media.Fail(http.StatusServiceUnavailable, "media_job_unavailable", "The media job store is unavailable."))
}

func (s *Server) OwnedJob(ctx context.Context, authority keyauthority.Authority, videoID string, op media.Operation) (*jobs.Record, *fault.Error) {
	id, err := uuid.Parse(videoID)
	if err != nil {
		return nil, fault.NotFoundError("video_not_found", "The video job does not exist.")
	}
	record, err := s.Deps.Media.Store.Get(ctx, id.String())
	if err != nil {
		return nil, JobError(err)
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
	if !authority.Allows("inference", record.RouteSlug, projectID, s.Now()) {
		return nil, fault.NotFoundError("video_not_found", "The video job does not exist.")
	}
	use := RetainedRetrieval
	if op == media.OpVideoDelete {
		use = RetainedHousekeeping
	}
	if e := RetainedContract(record.StrictContract, current, use); e != nil {
		return nil, e
	}
	return &record, nil
}

func (s *Server) jobRoute(ctx context.Context, slug string) (*string, routing.RouteFidelity, *fault.Error) {
	projectID, current, err := s.Deps.Media.Store.RouteAuthority(ctx, slug)
	if err != nil {
		return nil, routing.RouteFidelity{}, fault.ServerError(http.StatusServiceUnavailable, "route_unavailable", "The video job's route could not be read.")
	}
	return projectID, current, nil
}

func StrictVideoResourceContext(x *Call, record *jobs.Record, allowVariant bool) *fault.Error {
	if !record.StrictContract {
		return nil
	}
	if e := strictMediaHeaders(x); e != nil {
		return e
	}
	if x.semanticQueryInvalid || !allowVariant && len(x.semanticQuery) != 0 {
		return fault.InvalidRequest("target_capability", "This strict video resource does not qualify caller query settings.", nil)
	}
	return nil
}

func VideoJobRoute(record *jobs.Record) *routing.Route {
	route := &routing.Route{Slug: record.RouteSlug, Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed}}
	if record.StrictContract {
		route.Fidelity.Mode = routing.FidelityStrict
	}
	return route
}

func (s *Server) AdmitVideoRequest(parent context.Context, x *Call, authority keyauthority.Authority) (context.Context, func(), *fault.Error) {
	ttl := maxStreamDuration + jobs.LeaseSlack
	x.budgetGroupID = authority.BudgetGroupID
	ctx, cancel := context.WithTimeout(parent, ttl)
	var e *fault.Error
	x.lease, e = s.Deps.Admission.ReserveKey(ctx, x.AdmissionAuthority(authority), x.clientSurface(), ResourceEstimate, ttl, x.limitRoute())
	if e != nil {
		cancel()
		return nil, nil, e
	}
	return ctx, func() {
		quota.SettleKey(ctx, x.lease, x.dispatched, x.settledTokens(), s.Log)
		cancel()
	}, nil
}
