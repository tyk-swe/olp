package kernel

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/media/mediacontract"
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

func (s *Server) VideoJobCall(ctx context.Context, x *Call, record *jobs.Record, job media.JobCall) (*transport.Result, *AttemptFailure, events.AttemptFact) {
	plan, fact, refusal := s.planVideoJob(ctx, x, record, job, videoJobFact(record, s.Now()))
	if refusal != nil {
		return s.jobRefused(fact, refusal)
	}
	reservation, limited, skip := s.Deps.Admission.ReserveTarget(ctx, &plan.provider, &plan.slot, quota.TokenDemand{}, plan.target.Timeout+jobs.LeaseSlack, "", "", 0)
	if skip {
		return s.jobRefused(fact, &AttemptFailure{class: classLimitsUnavailable})
	}
	if limited != nil {
		return s.jobRefused(fact, rateLimitedTarget(limited))
	}
	dispatched := false
	defer func() { reservation.Settle(ctx, dispatched, nil) }()
	if _, deleting := job.(media.VideoDelete); deleting {
		if _, err := s.Deps.Media.Store.BeginDeletion(ctx, record.ID, jobs.Unfenced); err != nil {
			return s.jobRefused(fact, &AttemptFailure{class: ClassConnect})
		}
	}
	attemptCtx, trace := x.request.trace.Attempt(ctx, string(plan.target.Config.Kind), record.ProviderRevisionID, record.UpstreamModel)
	headers := http.Header{}
	trace.InjectUpstream(headers)
	callCtx, cancel := context.WithTimeout(attemptCtx, plan.target.Timeout+jobs.LeaseSlack)
	defer cancel()
	result, failure := s.Deps.Media.Transport.Do(callCtx, plan.target.Target, plan.call, transport.Options{Strict: plan.template != nil, Headers: headers})
	dispatched = failure == nil || failure.Dispatched
	timed := fact
	timed.Duration = s.Now().Sub(fact.StartedAt)
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

func (s *Server) planVideoJob(ctx context.Context, x *Call, record *jobs.Record, job media.JobCall, started events.AttemptFact) (videoJobPlan, events.AttemptFact, *AttemptFailure) {
	resolved, err := s.Deps.Media.Jobs.Target(ctx, *record)
	if err != nil {
		return videoJobPlan{}, started, &AttemptFailure{class: ClassConnect}
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
		return videoJobPlan{}, fact, &AttemptFailure{class: ClassProtocol}
	}
	template, err := videoJobContract(record, target, job)
	if err != nil {
		return videoJobPlan{}, fact, &AttemptFailure{class: ClassProtocol}
	}
	provider, slot := currentVideoLimits(x, resolved.Provider, resolved.Slot)
	fact.SlotID = slot.ID
	return videoJobPlan{target: target, call: call, template: template, provider: provider, slot: slot}, fact, nil
}

func videoCredentialFact(x *Call, provider routing.Provider, started events.AttemptFact) events.AttemptFact {
	if provider.CredentialSource != "caller" {
		return started
	}
	fact := started
	fact.CredentialSource, fact.CredentialID, fact.CredentialVersion = "caller", "", 0
	fact.BudgetExempt = x.callerCostExempt()
	return fact
}

func videoJobTarget(x *Call, record *jobs.Record, resolved jobs.Resolved) (jobs.Resolved, *AttemptFailure) {
	provider := resolved.Provider
	if e := x.checkCallerTarget(&provider, record.UpstreamModel); e != nil {
		return jobs.Resolved{}, &AttemptFailure{class: ClassCredential}
	}
	sensitive := &egress.Sensitive{}
	target := resolved
	target.Sensitive = sensitive
	if provider.CredentialSource != "caller" {
		return target, nil
	}
	secret, err := x.bindCallerSecret(provider.ID, record.UpstreamModel)
	if err != nil {
		return jobs.Resolved{}, &AttemptFailure{class: ClassCredential}
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

func currentVideoLimits(x *Call, stored routing.Provider, storedSlot routing.Slot) (routing.Provider, routing.Slot) {
	current, ok := x.request.release.Snapshot().FindProvider(stored.ID)
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

func (s *Server) videoJobFailed(record *jobs.Record, timed events.AttemptFact, failure *transport.Failure, trace *telemetry.AttemptTrace) (*transport.Result, *AttemptFailure, events.AttemptFact) {
	fact := timed
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
		artifact := MediaArtifact(result.Payload)
		if artifact == nil {
			return errors.New("native video content is unavailable")
		}
		_, err := artifact.BlobReference()
		return err
	}
	return nil
}

func (s *Server) videoJobBroken(timed events.AttemptFact, result *transport.Result, trace *telemetry.AttemptTrace) (*transport.Result, *AttemptFailure, events.AttemptFact) {
	if artifact := MediaArtifact(result.Payload); artifact != nil {
		if err := s.Deps.Staging.Remove(artifact.Handle); err != nil {
			s.Log.Warn("staged video content cleanup failed", "error", err)
		}
	}
	fact := timed
	fact.Class = ClassProtocol
	fact.Duration = s.Now().Sub(fact.StartedAt)
	fact.RecordEvidence(true)
	trace.Finish(fact.Class, result.Status)
	return nil, &AttemptFailure{class: ClassProtocol, Status: result.Status, dispatched: true}, fact
}

func (s *Server) videoJobSucceeded(record *jobs.Record, timed events.AttemptFact, result *transport.Result, trace *telemetry.AttemptTrace) (*transport.Result, *AttemptFailure, events.AttemptFact) {
	fact := timed
	fact.Class = "success"
	fact.Status = result.Status
	firstByte := result.FirstByte
	fact.FirstByte = &firstByte
	fact.Usage = mediaUsage(nil, result)
	fact.Committed = true
	fact.RecordEvidence(false)
	s.health.Record(record.ProviderID, fact, false)
	if u := fact.Usage; u != nil {
		trace.RecordUsage(u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
	}
	trace.Finish(fact.Class, fact.Status)
	return result, nil, fact
}

func (s *Server) jobRefused(fact events.AttemptFact, failure *AttemptFailure) (*transport.Result, *AttemptFailure, events.AttemptFact) {
	fact.Class = failure.class
	fact.Duration = s.Now().Sub(fact.StartedAt)
	fact.RecordEvidence(false)
	return nil, failure, fact
}

func (x *Call) NoteJobAttempt(fact events.AttemptFact) {
	x.facts = append(x.facts, fact)
	x.attemptCount, x.attemptVendor = len(x.facts), fact.VendorID
}

func (x *Call) NotePolls() {
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

func (s *Server) RefreshListRecord(ctx context.Context, x *Call, record jobs.Record) (jobs.Record, *fault.Error) {
	updated, fact, dispatched, e := s.refreshListRecord(ctx, x, record)
	x.dispatched = x.dispatched || dispatched
	if fact != nil {
		x.facts = append(x.facts, *fact)
	}
	return updated, e
}

func (s *Server) refreshListRecord(ctx context.Context, x *Call, record jobs.Record) (jobs.Record, *events.AttemptFact, bool, *fault.Error) {
	upstream, ok := record.Upstream()
	if !record.State.Polling() || !ok {
		return record, nil, false, nil
	}
	if record.StrictContract {
		if _, current, e := s.jobRoute(ctx, record.RouteSlug); e != nil || RetainedContract(true, current, RetainedRetrieval) != nil {
			return record, nil, false, nil
		}
	}
	result, transportFailure, fact := s.VideoJobCall(ctx, x, &record, media.VideoGet{ID: upstream})
	if transportFailure != nil {
		if transportFailure.quota != "" || transportFailure.class == classLimitsUnavailable {
			return record, &fact, false, transportFailure.Fault()
		}
		return record, &fact, transportFailure.dispatched, nil
	}
	update, ok := jobs.Observe(MediaVideo(result), s.Now())
	if !ok {
		return record, &fact, true, nil
	}
	updated, err := s.Deps.Media.Store.Refresh(ctx, record.ID, jobs.Unfenced, update)
	if err != nil {
		return record, &fact, true, nil
	}
	return updated, &fact, true, nil
}
