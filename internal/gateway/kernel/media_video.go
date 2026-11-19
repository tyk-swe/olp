package kernel

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/media/mediacontract"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
)

func (s *Server) videoCreate(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.mediaBegin(w, r)
	if done {
		return
	}
	form, parseError := s.ParseMediaForm(w, r, authority.ID, media.DefaultVideoReferenceLimit, 1)
	if parseError != nil {
		s.Gate.Release(r.Context())
		s.Refuse(x, w, parseError)
		return
	}
	request, failure := media.DecodeVideoCreate(form)
	if failure != nil {
		form.Cleanup()
		s.Gate.Release(r.Context())
		s.Refuse(x, w, mediaError(failure))
		return
	}
	x.family = wire.Format(request.Op())
	x.media = request
	x.estimate = mediaMultipartTokens
	defer s.Gate.Release(r.Context())
	defer s.cleanupUploads(request)

	jobID, err := uuid.NewV7()
	if err != nil {
		form.Cleanup()
		s.Refuse(x, w, jobError(err))
		return
	}
	localJobID := jobID.String()
	x.affinity = []byte(localJobID)
	if e := s.prepareMedia(r.Context(), x, authority); e != nil {
		form.Cleanup()
		s.Refuse(x, w, e)
		return
	}
	form.Disarm()
	attempt := x.attempts[0]
	provider, ok := x.request.release.Snapshot().FindProvider(attempt.ProviderID)
	if !ok {
		s.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider is currently able to serve `"+x.Route.Slug+"`."))
		return
	}
	if e := x.checkCallerTarget(&provider, attempt.UpstreamModel); e != nil {
		s.Refuse(x, w, e)
		return
	}
	slots := s.slots(x, attempt, &provider)
	if len(slots) == 0 {
		s.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider credential is currently able to serve `"+x.Route.Slug+"`."))
		return
	}

	overall := time.Duration(x.Route.OverallTimeout) * time.Millisecond
	ctx, cancel := x.routeContext(r.Context())
	defer cancel()
	var e *fault.Error
	admitted := x.AdmissionAuthority(authority)
	if x.lease, e = s.Deps.Admission.ReserveKeyCosted(ctx, &admitted, x.clientSurface(), x.estimate, overall, x.attemptCostReservation(authority, attempt), x.limitRoute()); e != nil {
		s.Refuse(x, w, e)
		return
	}
	defer s.settleAdmission(ctx, x)

	deadline, _ := ctx.Deadline()
	hold, slot, e := s.admitVideoSlot(ctx, x, attempt, &provider, slots, deadline)
	if hold == nil {
		s.Refuse(x, w, e)
		return
	}
	defer func() { hold.settle(ctx, x.dispatched, x.settledTokens()) }()

	reserved, jobErr := s.Deps.Media.Store.Reserve(ctx, jobs.Reservation{
		ID:                  localJobID,
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
	})
	if jobErr != nil {
		s.ReleaseHold(ctx, hold)
		s.Refuse(x, w, jobError(jobErr))
		return
	}

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
	s.health.Record(provider.ID, fact, hold.probed)
	commitCtx, stopCommit := ResourceCommitContext(ctx)
	defer stopCommit()
	if dispatchFailure != nil {
		s.Deps.Media.Jobs.Abandon(commitCtx, reserved.ID, dispatchFailure.class == classAmbiguous)
		out := &mediaOutcome{err: dispatchFailure.Fault(), committed: dispatchFailure.committed, cancelled: dispatchFailure.class == ClassCancelled}
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

func (s *Server) attachCreated(ctx context.Context, x *Call, reserved jobs.Record, result *transport.Result) *mediaOutcome {
	out := &mediaOutcome{result: result, committed: true, status: http.StatusCreated, localJob: reserved.ID}
	err := s.Deps.Media.Jobs.Attach(ctx, reserved, mediaVideo(result), &jobs.Caller{Secret: x.callerSecret, Sensitive: &x.sensitive})
	rejected, isRejected := errors.AsType[*jobs.Rejected](err)
	switch {
	case isRejected:
		out.err = fault.ServerError(http.StatusBadGateway, "provider_protocol_error", rejected.Message)
	case err != nil:
		out.err = fault.ServerError(http.StatusServiceUnavailable, "media_job_create_reconciliation_pending", "The video creation is being reconciled; check the job status shortly.")
	}
	return out
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

func (s *Server) ownedJob(ctx context.Context, authority keyauthority.Authority, videoID string, op media.Operation) (*jobs.Record, *fault.Error) {
	id, err := uuid.Parse(videoID)
	if err != nil {
		return nil, fault.NotFoundError("video_not_found", "The video job does not exist.")
	}
	record, err := s.Deps.Media.Store.Get(ctx, id.String())
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
	if !authority.Allows("inference", record.RouteSlug, projectID, s.Now()) {
		return nil, fault.NotFoundError("video_not_found", "The video job does not exist.")
	}
	use := RetainedRetrieval
	if op == media.OpVideoDelete {
		use = RetainedHousekeeping
	}
	if e := retainedContract(record.StrictContract, current, use); e != nil {
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

func strictVideoResourceContext(x *Call, record *jobs.Record, allowVariant bool) *fault.Error {
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

func videoJobRoute(record *jobs.Record) *routing.Route {
	route := &routing.Route{Slug: record.RouteSlug, Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed}}
	if record.StrictContract {
		route.Fidelity.Mode = routing.FidelityStrict
	}
	return route
}

func (s *Server) admitVideoRequest(parent context.Context, x *Call, authority keyauthority.Authority) (context.Context, func(), *fault.Error) {
	ttl := maxStreamDuration + jobs.LeaseSlack
	x.budgetGroupID = authority.BudgetGroupID
	ctx, cancel := context.WithTimeout(parent, ttl)
	var e *fault.Error
	x.lease, e = s.Deps.Admission.ReserveKey(ctx, x.AdmissionAuthority(authority), x.clientSurface(), resourceEstimate, ttl, x.limitRoute())
	if e != nil {
		cancel()
		return nil, nil, e
	}
	return ctx, func() {
		quota.SettleKey(ctx, x.lease, x.dispatched, x.settledTokens(), s.Log)
		cancel()
	}, nil
}

func (s *Server) videoJobCall(ctx context.Context, x *Call, record *jobs.Record, job media.JobCall) (*transport.Result, *AttemptFailure, events.AttemptFact) {
	fact := events.AttemptFact{
		TargetID:           record.ID,
		ProviderID:         record.ProviderID,
		ProviderRevisionID: record.ProviderRevisionID,
		UpstreamModel:      record.UpstreamModel,
		ModelFamily:        string(estimate.FamilyOf(record.UpstreamModel)),
		Mode:               "unary",
		StartedAt:          s.Now(),
	}
	if record.CredentialVersionID != nil {
		fact.CredentialID = *record.CredentialVersionID
	}
	target, err := s.Deps.Media.Jobs.Target(ctx, *record)
	if err != nil {
		return s.jobRefused(fact, &AttemptFailure{class: classConnect})
	}
	provider, slot := target.Provider, target.Slot
	if provider.CredentialSource == "caller" {
		fact.CredentialSource, fact.CredentialID, fact.CredentialVersion = "caller", "", 0
		fact.BudgetExempt = x.callerCostExempt()
	}

	if e := x.checkCallerTarget(&provider, record.UpstreamModel); e != nil {
		return s.jobRefused(fact, &AttemptFailure{class: classCredential})
	}
	var sensitive egress.Sensitive
	if provider.CredentialSource == "caller" {
		secret, err := x.bindCallerSecret(provider.ID, record.UpstreamModel)
		if err != nil {
			return s.jobRefused(fact, &AttemptFailure{class: classCredential})
		}
		sensitive.Add(string(secret))
		target.Secret, target.CallerCredential = secret, true
	}

	fact.VendorID = provider.VendorID
	call, encodeFailure := media.EncodeJob(job, target.Config)
	if encodeFailure != nil {
		return s.jobRefused(fact, &AttemptFailure{class: ClassProtocol})
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
			return s.jobRefused(fact, &AttemptFailure{class: ClassProtocol})
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
	reservation, refusal, skip := s.Deps.Admission.ReserveTarget(ctx, &provider, &slot, 0, target.Timeout+jobs.LeaseSlack, "")
	if skip {
		return s.jobRefused(fact, &AttemptFailure{class: classLimitsUnavailable})
	}
	if refusal != nil {
		return s.jobRefused(fact, rateLimitedTarget(refusal))
	}
	dispatched := false
	defer func() { reservation.Settle(ctx, dispatched, nil) }()
	if _, deleting := job.(media.VideoDelete); deleting {
		if _, err := s.Deps.Media.Store.BeginDeletion(ctx, record.ID, jobs.Unfenced); err != nil {
			return s.jobRefused(fact, &AttemptFailure{class: classConnect})
		}
	}
	attemptCtx, atr := x.request.trace.Attempt(ctx, string(target.Config.Kind), record.ProviderRevisionID, record.UpstreamModel)
	headers := http.Header{}
	atr.InjectUpstream(headers)
	callCtx, cancel := context.WithTimeout(attemptCtx, target.Timeout+jobs.LeaseSlack)
	defer cancel()
	target.Sensitive = &sensitive
	result, failure := s.Deps.Media.Transport.Do(callCtx, target.Target, call, transport.Options{Strict: strictTemplate != nil, Headers: headers})
	dispatched = failure == nil || failure.Dispatched
	fact.Duration = s.Now().Sub(fact.StartedAt)
	if failure != nil {
		fact.Class = mediaClass(failure)
		fact.Status = failure.Status
		if failure.RetryAfter > 0 {
			retry := failure.RetryAfter
			fact.RetryAfter = &retry
		}
		f := &AttemptFailure{
			class:      mediaClass(failure),
			Status:     failure.Status,
			retryAfter: failure.RetryAfter,
			upstream:   failure.Upstream,
			dispatched: failure.Dispatched,
			Acceptance: upstream.Evidence{Reached: failure.Dispatched, Status: failure.Status}.Acceptance(),
		}
		fact.RecordEvidence(f.billingUncertain())
		s.health.Record(record.ProviderID, fact, false)
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
				_, contractErr = artifact.BlobReference()
			}
		}
		if contractErr != nil {
			if artifact := mediaArtifact(result.Payload); artifact != nil {
				if err := s.Deps.Staging.Remove(artifact.Handle); err != nil {
					s.Log.Warn("staged video content cleanup failed", "error", err)
				}
			}
			fact.Class = ClassProtocol
			fact.Duration = s.Now().Sub(fact.StartedAt)
			fact.RecordEvidence(true)
			atr.Finish(fact.Class, result.Status)
			return nil, &AttemptFailure{class: ClassProtocol, Status: result.Status, dispatched: true}, fact
		}
	}
	fact.Class = "success"
	fact.Status = result.Status
	fb := result.FirstByte
	fact.FirstByte = &fb
	fact.Usage = mediaUsage(nil, result)
	fact.Committed = true
	fact.RecordEvidence(false)
	s.health.Record(record.ProviderID, fact, false)
	if u := fact.Usage; u != nil {
		atr.RecordUsage(u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
	}
	atr.Finish(fact.Class, fact.Status)
	return result, nil, fact
}

func (s *Server) jobRefused(fact events.AttemptFact, failure *AttemptFailure) (*transport.Result, *AttemptFailure, events.AttemptFact) {
	fact.Class = failure.class
	fact.Duration = s.Now().Sub(fact.StartedAt)
	fact.RecordEvidence(false)
	return nil, failure, fact
}

func (x *Call) noteJobAttempt(fact events.AttemptFact) {
	x.facts = append(x.facts, fact)
	x.attemptCount, x.attemptVendor = len(x.facts), fact.VendorID
}

func (x *Call) notePolls() {
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
