package jobs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
)

const (
	concurrency  = 4
	batch        = 16
	pollInterval = 5 * time.Second
	passBudget   = 10 * time.Minute
	retention    = 30 * 24 * time.Hour
	maxBackoff   = 300
)

type code string

func (c code) Error() string { return string(c) }

func (s *Service) Run(ctx context.Context) {
	workerhealth.Run(ctx, workerhealth.Loop{
		Task: workerhealth.TaskMediaReconciliation, Interval: pollInterval, Budget: passBudget,
		Health: s.store.db, Log: s.log, Pass: s.pass,
	})
}

func (s *Service) pass(ctx context.Context) (bool, error) {
	total := 0
	for total < batch {
		wanted := min(concurrency, batch-total)
		claimed, err := s.store.Claim(ctx, s.now(), wanted)
		if err != nil {
			return total > 0, err
		}
		var wg sync.WaitGroup
		for _, job := range claimed {
			wg.Go(func() { s.reconcile(ctx, job) })
		}
		wg.Wait()
		total += len(claimed)
		if len(claimed) < wanted {
			break
		}
	}
	return total > 0, nil
}

func changed(err error) bool {
	kind, ok := KindOf(err)
	return ok && kind == KindChanged
}

func (s *Service) reconcile(ctx context.Context, job Claimed) {
	record, err := s.step(ctx, job)
	if errors.Is(err, errClaimLost) {
		if err := s.store.Finish(ctx, job, s.now().Add(pollInterval), nil); err != nil && !changed(err) {
			s.log.Warn("media reconciliation claim release failed", "job_id", job.ID, "error", err)
		}
		return
	}
	next, failure := s.schedule(record, job.ReconciliationAttempts, err)
	if err := s.store.Finish(ctx, job, next, failure); err != nil {
		if !changed(err) {
			s.gap("media reconciliation checkpoint failed", "job_id", job.ID, "error", err)
		}
		return
	}
	if failure != nil {
		s.log.Warn("media reconciliation will retry", "job_id", job.ID, "error_class", *failure)
	}
}

func (s *Service) schedule(record Record, attempts int, err error) (time.Time, *string) {
	now := s.now()
	if err != nil {
		message := err.Error()
		failure := message[:min(len(message), 120)]
		return now.Add(time.Duration(min(5<<min(attempts, 6), maxBackoff)) * time.Second), &failure
	}
	if record.Lifecycle == Active && record.State.Polling() {
		return now.Add(pollInterval), nil
	}
	return nextTerminal(record, now), nil
}

func nextTerminal(record Record, now time.Time) time.Time {
	next := now.Add(24 * time.Hour)
	if record.Lifecycle != Active {
		return next
	}
	deadline := record.CreatedAt.Add(retention)
	if record.ExpiresAt != nil && record.ExpiresAt.Before(deadline) {
		deadline = *record.ExpiresAt
	}
	if deadline.After(next) {
		return next
	}
	if deadline.Before(now) {
		return now
	}
	return deadline
}

func refreshable(record Record, now time.Time) bool {
	switch record.Lifecycle {
	case CreateAmbiguous, CleanupPending, DeletePending:
		return true
	case Creating:
		return !record.NextReconciliationAt.After(now)
	case Active:
		return record.UpstreamJobID != nil && record.State.Polling()
	}
	return false
}

func mutation(err error) error {
	if err == nil || errors.Is(err, errClaimLost) {
		return err
	}
	return code("persistence_unavailable")
}

func (s *Service) retired(record Record) bool {
	now := s.now()
	return record.ExpiresAt != nil && !record.ExpiresAt.After(now) || record.CreatedAt.Before(now.Add(-retention))
}

func (s *Service) step(ctx context.Context, job Claimed) (Record, error) {
	fence := job.Fence()
	switch job.Lifecycle {
	case Creating:
		if job.UpstreamJobID == nil {
			if _, err := s.store.MarkAmbiguous(ctx, job.ID, fence, "upstream_create_outcome_unknown_after_restart"); err != nil {
				return Record{}, mutation(err)
			}
			return Record{}, code("upstream_create_outcome_unknown")
		}
		return s.cleanup(ctx, job, "stale_post_create_reservation")
	case CreateAmbiguous:
		if job.UpstreamJobID == nil {
			return Record{}, code("upstream_create_outcome_unknown")
		}
		return s.cleanup(ctx, job, "ambiguous_create_has_cleanup_identity")
	case Active:
		if !s.retired(job.Record) {
			return s.execute(ctx, job, job.Record)
		}
		deleting, err := s.store.BeginDeletion(ctx, job.ID, fence)
		if err != nil {
			return Record{}, mutation(err)
		}
		return s.settle(ctx, job, deleting)
	}
	return s.settle(ctx, job, job.Record)
}

func (s *Service) cleanup(ctx context.Context, job Claimed, reason string) (Record, error) {
	marked, err := s.store.MarkCleanup(ctx, job.ID, job.Fence(), *job.UpstreamJobID, reason)
	if err != nil {
		return Record{}, mutation(err)
	}
	return s.settle(ctx, job, marked)
}

func (s *Service) settle(ctx context.Context, job Claimed, record Record) (Record, error) {
	if record.Lifecycle == Deleted {
		return record, nil
	}
	return s.execute(ctx, job, record)
}

func failureCode(err error) error {
	failure, ok := errors.AsType[upstreamFailure](err)
	if !ok {
		return err
	}
	switch failure.Class {
	case media.ClassProtocol:
		return code("provider_protocol_error")
	case media.ClassTimeout, media.ClassRateLimit, media.ClassUpstreamServer, media.ClassUpstreamClient,
		media.ClassCredential, media.ClassCancelled:
		return code(failure.Class)
	}
	return code("connect")
}

func (s *Service) execute(ctx context.Context, job Claimed, record Record) (Record, error) {
	upstream, ok := record.Upstream()
	if !ok {
		return Record{}, code("media_job_upstream_id_unavailable")
	}
	resolved, err := s.Target(ctx, record)
	if err != nil {
		return Record{}, err
	}
	owned, err := s.store.Extend(ctx, job, s.now().Add(resolved.Timeout+LeaseSlack))
	if err != nil {
		return Record{}, code("persistence_unavailable")
	}
	if !owned {
		return Record{}, errClaimLost
	}
	if record.Lifecycle != Active {
		result, err := s.send(ctx, resolved, record, &media.Request{Op: media.OpVideoDelete, JobID: upstream, Route: record.RouteSlug})
		switch {
		case deleteConfirmed(result, err):
			deleted, err := s.store.Finalize(ctx, record.ID, job.Fence())
			return deleted, mutation(err)
		case err != nil:
			return Record{}, failureCode(err)
		}
		return Record{}, code("video_delete_not_confirmed")
	}
	result, err := s.send(ctx, resolved, record, &media.Request{Op: media.OpVideoGet, JobID: upstream, Route: record.RouteSlug})
	if err != nil {
		return Record{}, failureCode(err)
	}
	if record.StrictContract && !media.ValidStrictVideoJobSource(result.Source, upstream, record.UpstreamModel) {
		return Record{}, code("provider_protocol_error")
	}
	update, ok := Observe(result.Video, s.now())
	if !ok {
		return Record{}, code("provider_protocol_error")
	}
	refreshed, err := s.store.Refresh(ctx, record.ID, job.Fence(), update)
	return refreshed, mutation(err)
}
