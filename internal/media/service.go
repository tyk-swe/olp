package media

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

// Reconciliation concurrency and pacing match the reference worker.
const (
	ReconciliationConcurrency = 4
	ReconciliationBatch       = 16
	ReconciliationLeaseSlack  = 60 * time.Second
	reconcilePollInterval     = 5 * time.Second
)

var errClaimLost = errors.New("reconciliation_claim_lost")

// Service owns durable media job work: create attachment, client-facing
// refreshes, and the autonomous reconciliation loop.
type Service struct {
	Pool         *pgxpool.Pool
	Keys         *secrets.KeyRing
	Installation string
	Transport    *Transport
	// Revoked reports whether a credential version was explicitly revoked as
	// of the last authority read. A nil or stale authority must fail closed.
	Revoked func(credentialID string) bool
	Log     *slog.Logger
	// Gaps counts reconciliation steps that could not be checkpointed; the
	// worker counter makes unattended retries observable.
	Gaps *atomic.Uint64
	Now  func() time.Time
}

// Pass summarizes one reconciliation sweep.
type Pass struct {
	Claimed   int
	Completed int
	Failed    int
	HandedOff int
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// RecordGap counts a reconciliation step that could not be checkpointed.
func (s *Service) RecordGap() {
	if s.Gaps != nil {
		s.Gaps.Add(1)
	}
}

// checkpoint records one reconciliation pass in worker_task_health.
func (s *Service) checkpoint(ctx context.Context, outcome usage.Outcome, progress bool) {
	if s.Pool == nil {
		return
	}
	if err := usage.CheckpointTask(ctx, s.Pool, usage.TaskMediaReconciliation, outcome, progress); err != nil {
		s.log().Debug("media reconciliation checkpoint failed", "error", err)
	}
}

// AttachWithRetry persists the upstream identity with bounded retry; the
// create path must not abandon the reservation on a transient write failure.
func (s *Service) AttachWithRetry(ctx context.Context, id, upstreamJobID string, update JobUpdate) (JobRecord, error) {
	const attempts = 3
	for attempt := 0; ; attempt++ {
		record, err := AttachUpstream(ctx, s.Pool, id, upstreamJobID, update)
		if err == nil {
			return record, nil
		}
		var jobErr *JobError
		if !errors.As(err, &jobErr) || jobErr.Kind != JobErrorDatabase || attempt+1 >= attempts {
			return JobRecord{}, err
		}
		select {
		case <-ctx.Done():
			return JobRecord{}, err
		case <-time.After(time.Duration(25*(attempt+1)) * time.Millisecond):
		}
	}
}

// FinalizeDeletion applies the tombstone and confirms the row landed in the
// deleted lifecycle.
func (s *Service) FinalizeDeletion(ctx context.Context, id string) (bool, error) {
	finalized, err := FinalizeDeletion(ctx, s.Pool, id)
	if err != nil {
		return false, err
	}
	if finalized {
		return true, nil
	}
	record, err := Job(ctx, s.Pool, id)
	if err != nil {
		return false, err
	}
	return record.Lifecycle == LifecycleDeleted, nil
}

// JobTarget retains the historical connection and quota identities together.
type JobTarget struct {
	Target
	Provider runtime.Provider
	Slot     runtime.Slot
}

// JobTarget reconstructs the historical provider target a retained job was
// pinned to. Every failure path returns a short error class rather than a
// message so no secret or config detail is ever persisted on the job.
func (s *Service) JobTarget(ctx context.Context, record *JobRecord) (*JobTarget, time.Duration, string) {
	if record == nil {
		return nil, 0, "media_job_runtime_unavailable"
	}
	// The pinned generation must still be retained and must contain the exact
	// provider revision the reservation recorded.
	var raw []byte
	err := s.Pool.QueryRow(ctx,
		"SELECT snapshot FROM olp_go.runtime_releases WHERE id = $1", record.RuntimeGenerationID).Scan(&raw)
	if err != nil {
		return nil, 0, "media_job_runtime_unavailable"
	}
	var snapshot runtime.Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, 0, "media_job_runtime_unavailable"
	}
	provider, ok := snapshot.Providers[record.ProviderID]
	if !ok || provider.RevisionID != record.ProviderRevisionID {
		return nil, 0, "media_job_runtime_unavailable"
	}
	route, ok := snapshot.Routes[record.RouteSlug]
	if !ok {
		return nil, 0, "media_job_route_invalid"
	}
	routeTimeout := time.Duration(route.OverallTimeout) * time.Millisecond

	var selected *runtime.Slot
	for i := range provider.Slots {
		slot := &provider.Slots[i]
		if record.SlotID != nil {
			if slot.ID == *record.SlotID {
				selected = slot
				break
			}
		} else if (slot.CredentialID == nil && record.CredentialVersionID == nil) ||
			(slot.CredentialID != nil && record.CredentialVersionID != nil && *slot.CredentialID == *record.CredentialVersionID) {
			// Legacy jobs can recover a slot only when the identity is unique.
			if selected != nil {
				return nil, 0, "media_job_slot_unavailable"
			}
			selected = slot
		}
	}
	if selected == nil {
		return nil, 0, "media_job_slot_unavailable"
	}
	config := provider.Connector()
	var secret []byte
	if connectors.SecretRequired(config.AuthMode) {
		if record.CredentialVersionID == nil {
			return nil, 0, "media_job_runtime_unavailable"
		}
		credentialID := *record.CredentialVersionID
		// The pinned release must still reference this exact credential
		// version, and a current explicit revocation always wins over a
		// retained historical reference.
		if !credentialReferenced(&provider, credentialID) {
			return nil, 0, "media_job_runtime_unavailable"
		}
		if s.Revoked == nil || s.Revoked(credentialID) {
			return nil, 0, "media_job_credential_revoked"
		}
		if s.Keys == nil {
			return nil, 0, "media_job_runtime_unavailable"
		}
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			return nil, 0, "media_job_runtime_unavailable"
		}
		secret, err = s.Keys.Read(ctx, tx, s.Installation, credentialID, "provider_credential")
		tx.Rollback(ctx)
		if err != nil || len(secret) == 0 {
			return nil, 0, "media_job_runtime_unavailable"
		}
	} else if record.CredentialVersionID != nil && s.Revoked != nil && s.Revoked(*record.CredentialVersionID) {
		return nil, 0, "media_job_credential_revoked"
	}
	return &JobTarget{Target: Target{Config: config, Model: config.Model(record.UpstreamModel), Secret: secret}, Provider: provider, Slot: *selected}, routeTimeout, ""
}

// credentialReferenced reports whether the pinned provider entry still names
// this credential among its slots or as the active credential.
func credentialReferenced(provider *runtime.Provider, credentialID string) bool {
	if provider.ActiveCredential != nil && *provider.ActiveCredential == credentialID {
		return true
	}
	for i := range provider.Slots {
		if provider.Slots[i].CredentialID != nil && *provider.Slots[i].CredentialID == credentialID {
			return true
		}
	}
	return false
}

// ReconcileOnce claims a bounded batch of restart-safe jobs and processes
// them with bounded concurrency.
func (s *Service) ReconcileOnce(ctx context.Context, limit int) (*Pass, error) {
	pass := &Pass{}
	claimed := 0
	for claimed < limit {
		wanted := min(ReconciliationConcurrency, limit-claimed)
		records, err := ClaimJobs(ctx, s.Pool, s.now(), wanted)
		if err != nil {
			return pass, err
		}
		claimed += len(records)
		pass.Claimed += len(records)
		var wg sync.WaitGroup
		results := make(chan reconciliationOutcome, len(records))
		for _, record := range records {
			wg.Go(func() {
				results <- s.reconcileClaimed(ctx, record)
			})
		}
		wg.Wait()
		close(results)
		for outcome := range results {
			switch outcome {
			case outcomeCompleted:
				pass.Completed++
			case outcomeHandedOff:
				pass.HandedOff++
			default:
				pass.Failed++
			}
		}
		if len(records) < wanted {
			break
		}
	}
	return pass, nil
}

type reconciliationOutcome int

const (
	outcomeCompleted reconciliationOutcome = iota
	outcomeFailed
	outcomeHandedOff
)

type reconciliationError string

func (e reconciliationError) Error() string { return string(e) }

// reconcileClaimed runs one claimed job and checkpoints the result.
func (s *Service) reconcileClaimed(ctx context.Context, record JobRecord) reconciliationOutcome {
	if record.ReconciliationClaimID == nil {
		s.RecordGap()
		return outcomeFailed
	}
	claimID := *record.ReconciliationClaimID
	outcome := s.reconcileOperation(ctx, &record, claimID)
	if errors.Is(outcome, errClaimLost) {
		return outcomeHandedOff
	}
	now := s.now()
	var next time.Time
	var errorClass *string
	if outcome == nil {
		if record.Lifecycle == LifecycleActive &&
			(record.State == StateQueued || record.State == StateRunning) {
			next = now.Add(5 * time.Second)
		} else {
			next = now.Add(24 * time.Hour)
		}
	} else {
		code := outcome.Error()
		if len(code) > 120 {
			code = code[:120]
		}
		errorClass = &code
		exponent := min(record.ReconciliationAttempts, 6)
		next = now.Add(time.Duration(min(int64(5)*(int64(1)<<exponent), 300)) * time.Second)
	}
	if err := FinishReconciliation(ctx, s.Pool, record.ID, claimID, next, errorClass); err != nil {
		var jobErr *JobError
		if errors.As(err, &jobErr) && jobErr.Kind == JobErrorPrecondition {
			// The lease moved to another worker during the operation; the
			// rejected checkpoint is a benign handoff, not a gap.
			return outcomeHandedOff
		}
		s.RecordGap()
		s.log().Error("media reconciliation checkpoint failed", "job_id", record.ID, "error", err)
		return outcomeFailed
	}
	if errorClass != nil {
		s.log().Warn("media reconciliation will retry", "job_id", record.ID, "error_class", *errorClass)
		return outcomeFailed
	}
	return outcomeCompleted
}

// mutationFailure maps a claim-fenced mutation result onto the reconciliation
// outcome: a refused fence is a benign handoff while real database and
// missing-record failures keep persistence reporting.
func mutationFailure(err error) error {
	if errors.Is(err, errClaimLost) {
		return errClaimLost
	}
	return reconciliationError("persistence_unavailable")
}

// reconcileOperation performs the lifecycle transition or upstream call one
// claimed job needs.
func (s *Service) reconcileOperation(ctx context.Context, record *JobRecord, claimID string) error {
	switch record.Lifecycle {
	case LifecycleCreating:
		if record.UpstreamJobID != nil {
			updated, err := markCreateCleanupPendingClaimed(ctx, s.Pool, record.ID, claimID, *record.UpstreamJobID,
				"stale_post_create_reservation")
			if err != nil {
				return mutationFailure(err)
			}
			*record = updated
		} else {
			if _, err := markCreateAmbiguousClaimed(ctx, s.Pool, record.ID, claimID,
				"upstream_create_outcome_unknown_after_restart"); err != nil {
				return mutationFailure(err)
			}
			return reconciliationError("upstream_create_outcome_unknown")
		}
	case LifecycleCreateAmbiguous:
		if record.UpstreamJobID == nil {
			return reconciliationError("upstream_create_outcome_unknown")
		}
		updated, err := markCreateCleanupPendingClaimed(ctx, s.Pool, record.ID, claimID, *record.UpstreamJobID,
			"ambiguous_create_has_cleanup_identity")
		if err != nil {
			return mutationFailure(err)
		}
		*record = updated
	case LifecycleDeleted:
		return nil
	case LifecycleActive, LifecycleCreateCleanupPending, LifecycleDeletePending:
	}

	if record.Lifecycle == LifecycleActive &&
		(record.ExpiresAt != nil && !record.ExpiresAt.After(s.now()) ||
			record.CreatedAt.Before(s.now().Add(-30*24*time.Hour))) {
		updated, err := beginDeletionClaimed(ctx, s.Pool, record.ID, claimID)
		if err != nil {
			return mutationFailure(err)
		}
		*record = updated
	}
	// A concurrent client delete may have finished the tombstone while the
	// claim was held; there is no upstream call left to confirm.
	if record.Lifecycle == LifecycleDeleted {
		return nil
	}

	return s.executeReconciliation(ctx, record, claimID)
}

// executeReconciliation performs the upstream poll or delete a claimed job
// needs, holding the claim lease for the whole upstream call.
func (s *Service) executeReconciliation(ctx context.Context, record *JobRecord, claimID string) error {
	upstreamID := ""
	if record.UpstreamJobID != nil {
		upstreamID = *record.UpstreamJobID
	}
	if !ValidUpstreamJobID(upstreamID) {
		return reconciliationError("media_job_upstream_id_unavailable")
	}
	isDelete := record.Lifecycle != LifecycleActive
	op := OpVideoGet
	if isDelete {
		op = OpVideoDelete
	}
	target, routeTimeout, code := s.JobTarget(ctx, record)
	if target == nil {
		return reconciliationError(code)
	}
	// Revalidate ownership and bound the lease across the upstream call: a
	// second replica may have reclaimed the row while the target was rebuilt.
	leaseUntil := s.now().Add(routeTimeout + ReconciliationLeaseSlack)
	owned, err := ExtendClaim(ctx, s.Pool, record.ID, claimID, leaseUntil)
	if err != nil {
		return reconciliationError("persistence_unavailable")
	}
	if !owned {
		return errClaimLost
	}
	callCtx, cancel := context.WithTimeout(ctx, routeTimeout+ReconciliationLeaseSlack)
	defer cancel()
	call, failure := Encode(&Request{Op: op, JobID: upstreamID, Route: record.RouteSlug}, record.UpstreamModel)
	if failure != nil {
		return reconciliationError("media_job_operation_invalid")
	}
	result, transportFailure := s.Transport.Do(callCtx, target.Target, call, nil)
	if transportFailure != nil {
		if isDelete && transportFailure.Status == 404 {
			return s.confirmDeletion(ctx, record, claimID)
		}
		return reconciliationError(failureClassCode(transportFailure))
	}
	if !isDelete {
		if result.Video == nil {
			return reconciliationError("provider_protocol_error")
		}
		state, ok := VideoState(result.Video.Status)
		if !ok {
			return reconciliationError("provider_protocol_error")
		}
		updated, err := refreshJobClaimed(ctx, s.Pool, record.ID, claimID, JobUpdate{
			State:            state,
			ProgressPercent:  result.Video.Progress,
			ContentAvailable: result.Video.Status == "completed",
			ExpiresAt:        UnixTime(result.Video.ExpiresAt),
			ErrorClass:       result.Video.ErrorCode,
			LastPolledAt:     s.now(),
		})
		if err != nil {
			return mutationFailure(err)
		}
		*record = updated
		return nil
	}
	if result.Deleted == nil || !result.Deleted.Deleted {
		return reconciliationError("video_delete_not_confirmed")
	}
	return s.confirmDeletion(ctx, record, claimID)
}

func (s *Service) confirmDeletion(ctx context.Context, record *JobRecord, claimID string) error {
	if err := finalizeDeletionClaimed(ctx, s.Pool, record.ID, claimID); err != nil {
		return mutationFailure(err)
	}
	record.Lifecycle = LifecycleDeleted
	return nil
}

// VideoState maps an upstream status onto the durable job state.
func VideoState(status string) (State, bool) {
	switch status {
	case "queued":
		return StateQueued, true
	case "in_progress":
		return StateRunning, true
	case "completed":
		return StateSucceeded, true
	case "failed":
		return StateFailed, true
	}
	return "", false
}

// failureClassCode reduces a transport failure to a bounded error class for
// the reconciliation ledger.
func failureClassCode(f *Failure) string {
	switch f.Class {
	case ClassTimeout:
		return "timeout"
	case ClassRateLimit:
		return "rate_limit"
	case ClassUpstreamServer:
		return "upstream_server"
	case ClassUpstreamClient:
		return "upstream_client"
	case ClassCredential:
		return "credential"
	case ClassProtocol:
		return "provider_protocol_error"
	case ClassCancelled:
		return "cancelled"
	default:
		return "connect"
	}
}

// RunReconciler is the autonomous reconciliation supervisor loop.
func (s *Service) RunReconciler(ctx context.Context) {
	ticker := time.NewTicker(reconcilePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pass, err := s.ReconcileOnce(ctx, ReconciliationBatch)
			if err != nil {
				s.log().Warn("media reconciliation pass failed", "error", err)
				s.checkpoint(ctx, usage.OutcomeFailure, false)
				continue
			}
			s.checkpoint(ctx, usage.OutcomeSuccess, pass.Claimed > 0)
			if pass.Claimed > 0 {
				s.log().Info("media reconciliation pass completed",
					"claimed", pass.Claimed, "completed", pass.Completed, "failed", pass.Failed)
			}
		}
	}
}
