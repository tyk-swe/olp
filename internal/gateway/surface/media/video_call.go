package media

import (
	"context"
	"errors"
	"net/http"
	"time"

	mediacontract "github.com/tyk-swe/olp/internal/contracts/media"
	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/telemetry"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
)

type videoJobPlan struct {
	target   jobs.Resolved
	call     *media.UpstreamCall
	template *mediacontract.Template
	provider routing.Provider
	slot     routing.Slot
}

func (s surface) videoJobCall(ctx context.Context, x *kernel.Call, record *jobs.Record, job media.JobCall) (*transport.Result, *kernel.AttemptFailure, events.AttemptFact) {
	plan, fact, refusal := s.planVideoJob(ctx, x, record, job, videoJobFact(record, s.kernel.Now()))
	if refusal != nil {
		return s.jobRefused(fact, refusal)
	}
	reservation, limited, skip := s.kernel.Deps.Admission.ReserveTarget(ctx, &plan.provider, &plan.slot, quota.TokenDemand{}, plan.target.Timeout+jobs.LeaseSlack, "", "", 0)
	if skip {
		return s.jobRefused(fact, kernel.AttemptFailureFor(kernel.ClassLimitsUnavailable))
	}
	if limited != nil {
		return s.jobRefused(fact, kernel.RateLimitedTarget(limited))
	}
	dispatched := false
	defer func() { reservation.Settle(ctx, dispatched, nil) }()
	if _, deleting := job.(media.VideoDelete); deleting {
		if _, err := s.deps.Store.BeginDeletion(ctx, record.ID, jobs.Unfenced); err != nil {
			return s.jobRefused(fact, kernel.AttemptFailureFor(kernel.ClassConnect))
		}
	}
	attemptCtx, trace := x.StartAttemptTrace(ctx, string(plan.target.Config.Kind), record.ProviderRevisionID, record.UpstreamModel)
	headers := http.Header{}
	trace.InjectUpstream(headers)
	callCtx, cancel := context.WithTimeout(attemptCtx, plan.target.Timeout+jobs.LeaseSlack)
	defer cancel()
	result, failure := s.deps.Transport.Do(callCtx, plan.target.Target, plan.call, transport.Options{Strict: plan.template != nil, Headers: headers})
	dispatched = failure == nil || failure.Dispatched
	timed := fact
	timed.Duration = s.kernel.Now().Sub(fact.StartedAt)
	if failure != nil {
		return s.videoJobFailed(record, timed, failure, trace)
	}
	if plan.template != nil && strictVideoResult(plan.template, job, record, result) != nil {
		return s.videoJobBroken(timed, result, trace)
	}
	return s.videoJobSucceeded(record, timed, result, trace)
}

func videoJobFact(record *jobs.Record, now time.Time) events.AttemptFact {
	fact := events.AttemptFact{
		TargetID:           record.ID,
		ProviderID:         record.ProviderID,
		ProviderRevisionID: record.ProviderRevisionID,
		UpstreamModel:      record.UpstreamModel,
		ModelFamily:        string(estimate.FamilyOf(record.UpstreamModel)),
		Mode:               "unary",
		StartedAt:          now,
	}
	if record.CredentialVersionID != nil {
		fact.CredentialID = *record.CredentialVersionID
	}
	return fact
}

func (s surface) planVideoJob(ctx context.Context, x *kernel.Call, record *jobs.Record, job media.JobCall, started events.AttemptFact) (videoJobPlan, events.AttemptFact, *kernel.AttemptFailure) {
	resolved, err := s.deps.Jobs.Target(ctx, *record)
	if err != nil {
		return videoJobPlan{}, started, kernel.AttemptFailureFor(kernel.ClassConnect)
	}
	credited := videoCredentialFact(x, resolved.Provider, started)
	target, refusal := videoJobTarget(x, record, resolved)
	if refusal != nil {
		return videoJobPlan{}, credited, refusal
	}
	fact := credited
	fact.VendorID = resolved.Provider.VendorID
	call, encodeFailure := media.EncodeJob(job, target.Config)
	if encodeFailure != nil {
		return videoJobPlan{}, fact, kernel.AttemptFailureFor(kernel.ClassProtocol)
	}
	template, err := videoJobContract(record, target, job)
	if err != nil {
		return videoJobPlan{}, fact, kernel.AttemptFailureFor(kernel.ClassProtocol)
	}
	provider, slot := currentVideoLimits(x, resolved.Provider, resolved.Slot)
	fact.SlotID = slot.ID
	return videoJobPlan{target: target, call: call, template: template, provider: provider, slot: slot}, fact, nil
}

func videoCredentialFact(x *kernel.Call, provider routing.Provider, started events.AttemptFact) events.AttemptFact {
	if provider.CredentialSource != "caller" {
		return started
	}
	fact := started
	fact.CredentialSource, fact.CredentialID, fact.CredentialVersion = "caller", "", 0
	fact.BudgetExempt = x.CallerCostExempt()
	return fact
}

func videoJobTarget(x *kernel.Call, record *jobs.Record, resolved jobs.Resolved) (jobs.Resolved, *kernel.AttemptFailure) {
	provider := resolved.Provider
	if e := x.CheckCallerTarget(&provider, record.UpstreamModel); e != nil {
		return jobs.Resolved{}, kernel.AttemptFailureFor(kernel.ClassCredential)
	}
	sensitive := &egress.Sensitive{}
	target := resolved
	target.Sensitive = sensitive
	if provider.CredentialSource != "caller" {
		return target, nil
	}
	secret, err := x.BindCallerSecret(provider.ID, record.UpstreamModel)
	if err != nil {
		return jobs.Resolved{}, kernel.AttemptFailureFor(kernel.ClassCredential)
	}
	sensitive.Add(string(secret))
	target.Secret, target.CallerCredential = secret, true
	return target, nil
}

func videoJobContract(record *jobs.Record, target jobs.Resolved, job media.JobCall) (*mediacontract.Template, error) {
	if !record.StrictContract {
		return nil, nil
	}
	return mediacontract.Compile(mediacontract.Config{
		Provider: target.Config, ProviderID: record.ProviderID,
		RevisionID: record.ProviderRevisionID, Model: record.UpstreamModel,
		Operation: string(job.Operation()),
	})
}

func currentVideoLimits(x *kernel.Call, stored routing.Provider, storedSlot routing.Slot) (routing.Provider, routing.Slot) {
	current, ok := x.Snapshot().FindProvider(stored.ID)
	if !ok {
		return stored, storedSlot
	}
	provider := stored
	provider.Limits = current.Limits
	for _, currentSlot := range current.Slots {
		if currentSlot.ID == storedSlot.ID {
			slot := storedSlot
			slot.RequestsPerMinute = currentSlot.RequestsPerMinute
			slot.TokensPerMinute = currentSlot.TokensPerMinute
			slot.InputTokensPerMinute = currentSlot.InputTokensPerMinute
			slot.OutputTokensPerMinute = currentSlot.OutputTokensPerMinute
			slot.MaxConcurrency = currentSlot.MaxConcurrency
			return provider, slot
		}
	}
	return provider, storedSlot
}

func (s surface) videoJobFailed(record *jobs.Record, timed events.AttemptFact, failure *transport.Failure, trace *telemetry.AttemptTrace) (*transport.Result, *kernel.AttemptFailure, events.AttemptFact) {
	fact := timed
	fact.Class = mediaClass(failure)
	fact.Status = failure.Status
	if failure.RetryAfter > 0 {
		retry := failure.RetryAfter
		fact.RetryAfter = &retry
	}
	f := kernel.NewAttemptFailure(kernel.AttemptFailureSpec{
		Class:      mediaClass(failure),
		Status:     failure.Status,
		RetryAfter: failure.RetryAfter,
		Upstream:   failure.Upstream,
		Dispatched: failure.Dispatched,
		Acceptance: upstream.Evidence{Reached: failure.Dispatched, Status: failure.Status}.Acceptance(),
	})
	fact.RecordEvidence(f.BillingUncertain())
	s.kernel.RecordProviderHealth(record.ProviderID, fact, false)
	trace.Finish(fact.Class, fact.Status)
	return nil, f, fact
}

func strictVideoResult(template *mediacontract.Template, job media.JobCall, record *jobs.Record, result *transport.Result) error {
	switch job.(type) {
	case media.VideoGet, media.VideoDelete:
		if !result.Source.Valid() || record.UpstreamJobID == nil {
			return errors.New("native video source is unavailable")
		}
		if _, err := template.JSONResult(template.Descriptor("unary"), result.Source); err != nil {
			return err
		}
		id, _ := result.Source.Root().Lookup("id")
		if actual, _ := id.Text(); actual != *record.UpstreamJobID {
			return errors.New("native video identity changed")
		}
	case media.VideoContent:
		artifact := mediaArtifact(result.Payload)
		if artifact == nil {
			return errors.New("native video content is unavailable")
		}
		_, err := artifact.BlobReference()
		return err
	}
	return nil
}

func (s surface) videoJobBroken(timed events.AttemptFact, result *transport.Result, trace *telemetry.AttemptTrace) (*transport.Result, *kernel.AttemptFailure, events.AttemptFact) {
	if artifact := mediaArtifact(result.Payload); artifact != nil {
		if err := s.kernel.Deps.Staging.Remove(artifact.Handle); err != nil {
			s.kernel.Log.Warn("staged video content cleanup failed", "error", err)
		}
	}
	fact := timed
	fact.Class = kernel.ClassProtocol
	fact.Duration = s.kernel.Now().Sub(fact.StartedAt)
	fact.RecordEvidence(true)
	trace.Finish(fact.Class, result.Status)
	return nil, kernel.NewAttemptFailure(kernel.AttemptFailureSpec{Class: kernel.ClassProtocol, Status: result.Status, Dispatched: true}), fact
}

func (s surface) videoJobSucceeded(record *jobs.Record, timed events.AttemptFact, result *transport.Result, trace *telemetry.AttemptTrace) (*transport.Result, *kernel.AttemptFailure, events.AttemptFact) {
	fact := timed
	fact.Class = kernel.ClassSuccess
	fact.Status = result.Status
	firstByte := result.FirstByte
	fact.FirstByte = &firstByte
	fact.Usage = mediaUsage(nil, result)
	fact.Committed = true
	fact.RecordEvidence(false)
	s.kernel.RecordProviderHealth(record.ProviderID, fact, false)
	if u := fact.Usage; u != nil {
		trace.RecordUsage(u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
	}
	trace.Finish(fact.Class, fact.Status)
	return result, nil, fact
}

func (s surface) jobRefused(fact events.AttemptFact, failure *kernel.AttemptFailure) (*transport.Result, *kernel.AttemptFailure, events.AttemptFact) {
	fact.Class = failure.Class()
	fact.Duration = s.kernel.Now().Sub(fact.StartedAt)
	fact.RecordEvidence(false)
	return nil, failure, fact
}

func (s surface) refreshListRecord(ctx context.Context, x *kernel.Call, record jobs.Record) (jobs.Record, *fault.Error) {
	updated, fact, dispatched, e := s.pollListRecord(ctx, x, record)
	x.RecordAttempt(fact, dispatched)
	return updated, e
}

func (s surface) pollListRecord(ctx context.Context, x *kernel.Call, record jobs.Record) (jobs.Record, *events.AttemptFact, bool, *fault.Error) {
	upstreamID, ok := record.Upstream()
	if !record.State.Polling() || !ok {
		return record, nil, false, nil
	}
	if record.StrictContract {
		if _, current, e := s.jobRoute(ctx, record.RouteSlug); e != nil || kernel.RetainedContract(true, current, kernel.RetainedRetrieval) != nil {
			return record, nil, false, nil
		}
	}
	result, transportFailure, fact := s.videoJobCall(ctx, x, &record, media.VideoGet{ID: upstreamID})
	if transportFailure != nil {
		if transportFailure.Quota() != "" || transportFailure.Class() == kernel.ClassLimitsUnavailable {
			return record, &fact, false, transportFailure.Fault()
		}
		return record, &fact, transportFailure.Dispatched(), nil
	}
	update, ok := jobs.Observe(mediaVideo(result), s.kernel.Now())
	if !ok {
		return record, &fact, true, nil
	}
	updated, err := s.deps.Store.Refresh(ctx, record.ID, jobs.Unfenced, update)
	if err != nil {
		return record, &fact, true, nil
	}
	return updated, &fact, true, nil
}
