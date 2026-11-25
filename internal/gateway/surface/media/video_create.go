package media

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func videoCreateUsage(request *media.Request) kernel.FixedUsageBound {
	if request == nil {
		return kernel.FixedUsageBound{}
	}
	seconds, video := request.VideoSeconds()
	if !video {
		return kernel.FixedUsageBound{}
	}
	return kernel.FixedUsageBound{
		Bounded: true,
		Usage: usage.AttemptUsage{
			Evidence:        usage.EvidenceUnobservedComplete,
			UsageQuantities: usage.UsageQuantities{MediaUnits: &seconds},
		},
	}
}

func (s surface) readVideoCreate(w http.ResponseWriter, r *http.Request, authority keyauthority.Authority) (*staging.Form, *media.Request, *fault.Error) {
	form, parseError := s.kernel.ParseMultipartForm(w, r, authority.ID, media.DefaultVideoReferenceLimit, 1)
	if parseError != nil {
		return nil, nil, parseError
	}
	request, failure := media.DecodeVideoCreate(form)
	if failure != nil {
		form.Cleanup()
		return nil, nil, mediaError(failure)
	}
	return form, request, nil
}

func (s surface) createVideoJob(w http.ResponseWriter, r *http.Request, x *kernel.Call, authority keyauthority.Authority, form *staging.Form, request *media.Request) {
	defer s.cleanupUploads(request)
	configureMediaCall(x, request, nil)

	jobID := ids.New()
	x.SetAffinity([]byte(jobID))
	if e := s.prepareMedia(r.Context(), x, authority, request); e != nil {
		form.Cleanup()
		s.kernel.Refuse(x, w, e)
		return
	}
	form.Disarm()
	target, e := s.kernel.FirstPlannedTarget(x)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}

	ctx, cancel := x.RouteContext(r.Context())
	defer cancel()
	if e = s.kernel.ReserveAttemptKeyCosted(ctx, x, authority, target.Attempt); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	defer s.kernel.SettleAdmission(ctx, x)

	deadline, _ := ctx.Deadline()
	hold, e := s.kernel.AdmitSingleTarget(ctx, x, target, quota.CombinedDemand(x.Estimate()), deadline)
	if hold == nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	defer s.kernel.SettleSingleHold(ctx, x, hold)

	reserved, jobErr := s.deps.Store.Reserve(ctx, videoReservation(x, authority, target.Attempt, hold.Slot, jobID, deadline))
	if jobErr != nil {
		s.kernel.ReleaseSingleHold(ctx, hold)
		s.kernel.Refuse(x, w, jobError(jobErr))
		return
	}
	s.submitVideo(ctx, w, x, request, target, hold, reserved, deadline)
}

func videoReservation(x *kernel.Call, authority keyauthority.Authority, attempt routing.Attempt, slot routing.Slot, jobID string, deadline time.Time) jobs.Reservation {
	return jobs.Reservation{
		ID:                  jobID,
		StrictContract:      x.Strict(),
		RuntimeGenerationID: x.Snapshot().Generation().ID,
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

func (s surface) submitVideo(ctx context.Context, w http.ResponseWriter, x *kernel.Call, request *media.Request, target kernel.SingleTarget, hold *kernel.SingleSlotHold, reserved jobs.Record, deadline time.Time) {
	dispatchCtx, dispatchCancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer dispatchCancel()
	planned := s.kernel.NewSingleAttemptFact(x, target.Attempt, hold)
	fact, result, dispatchFailure := s.mediaAttempt(dispatchCtx, w, x, request, target.Attempt, target.Provider, hold.Slot, planned)
	s.kernel.CompleteSingleAttempt(x, hold, target.Attempt, target.Provider.ID, fact, dispatchFailure)
	commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
	defer stopCommit()
	if dispatchFailure != nil {
		s.deps.Jobs.Abandon(commitCtx, reserved.ID, dispatchFailure.Class() == kernel.ClassAmbiguous)
		e := dispatchFailure.Fault()
		s.kernel.Finish(x, &kernel.Outcome{Err: e, Committed: dispatchFailure.Committed(), Cancelled: dispatchFailure.Class() == kernel.ClassCancelled}, e.Status)
		fault.WriteSurfaceError(w, e, "openai")
		return
	}
	if attachErr := s.attachCreated(commitCtx, x, reserved, result); attachErr != nil {
		s.kernel.Finish(x, &kernel.Outcome{Err: attachErr, Committed: true}, attachErr.Status)
		fault.WriteSurfaceError(w, attachErr, "openai")
		return
	}
	delivered := s.writeMediaResult(w, x, request, result, http.StatusCreated, reserved.ID)
	s.kernel.Finish(x, &delivered.outcome, delivered.status)
}

func (s surface) attachCreated(ctx context.Context, x *kernel.Call, reserved jobs.Record, result *transport.Result) *fault.Error {
	err := s.deps.Jobs.Attach(ctx, reserved, mediaVideo(result), &jobs.Caller{Secret: x.CallerSecret, Sensitive: x.SensitiveRef()})
	if err == nil {
		return nil
	}
	if rejected, ok := errors.AsType[*jobs.Rejected](err); ok {
		return fault.ServerError(http.StatusBadGateway, "provider_protocol_error", rejected.Message)
	}
	return fault.ServerError(http.StatusServiceUnavailable, "media_job_create_reconciliation_pending", "The video creation is being reconciled; check the job status shortly.")
}
