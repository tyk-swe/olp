package gateway

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

type SessionDebitLedger interface {
	Prepare(context.Context, debits.Intent, []limits.Owner) (debits.State, error)
	Admit(context.Context, string, string, []debits.Boundary) error
	MarkDispatched(context.Context, string, string) error
	MarkUnknown(context.Context, string, string) error
	ReleaseUndispatched(context.Context, string, string) error
	HasUnknownDebit(context.Context, string, time.Duration) (bool, error)
	Currency(context.Context) (string, error)
}

type durableSessionDebits struct {
	*debits.Store
	*debits.Syncer
	pool *pgxpool.Pool
}

type workflowModelLeases interface {
	MarkWorkflowModelDispatched(context.Context, string, string) error
	RenewWorkflowModelLease(context.Context, string, string) error
}

func DurableSessionDebits(pool *pgxpool.Pool, store *debits.Store, syncer *debits.Syncer) SessionDebitLedger {
	return durableSessionDebits{Store: store, Syncer: syncer, pool: pool}
}

func (d durableSessionDebits) SettleRecovery(ctx context.Context, result debits.Result) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := debits.SettleToolTx(ctx, tx, result); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (d durableSessionDebits) PreviousRecovery(ctx context.Context, runID, keyID string, ids []string) (bool, bool, error) {
	var settled, pending bool
	err := d.pool.QueryRow(ctx, `SELECT COALESCE(bool_or(state='settled'),false), COALESCE(bool_or(state IN ('prepared','ready','dispatched','outcome_unknown')),false)
		FROM olp.execution_debits WHERE run_id=$1 AND api_key_id=$2 AND kind='tool' AND id=ANY($3::uuid[])`, runID, keyID, ids).Scan(&settled, &pending)
	return settled, pending, err
}

func (d durableSessionDebits) Currency(ctx context.Context) (string, error) {
	var currency string
	err := d.pool.QueryRow(ctx, `SELECT COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton),'USD')`).Scan(&currency)
	return currency, err
}

type sessionDebit struct {
	id              string
	claim           string
	runID           string
	stepID          string
	targetID        string
	routeRevisionID string
	dispatched      bool
	leaseStarted    chan struct{}
}

func (d *sessionDebit) reference() *usage.ExecutionReference {
	if d == nil {
		return nil
	}
	return &usage.ExecutionReference{DispatchID: d.id, RunID: d.runID, StepID: d.stepID, TargetID: d.targetID, RouteRevisionID: d.routeRevisionID}
}

func (s *Server) monitorWorkflowModelLease(ctx context.Context, debit *sessionDebit) (context.Context, func() *fault.Error) {
	callCtx, cancel := context.WithCancel(ctx)
	result := make(chan *fault.Error, 1)
	done := make(chan struct{})
	ledger, ok := s.deps.Debits.(workflowModelLeases)
	if !ok || debit == nil {
		close(done)
		return callCtx, func() *fault.Error { cancel(); return nil }
	}
	go func() {
		defer close(done)
		select {
		case <-callCtx.Done():
			return
		case <-debit.leaseStarted:
		}
		ticker := time.NewTicker(debits.WorkflowModelLeaseDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-callCtx.Done():
				return
			case <-ticker.C:
				checkCtx, stop := context.WithTimeout(callCtx, time.Second)
				err := ledger.RenewWorkflowModelLease(checkCtx, debit.id, debit.claim)
				stop()
				if err != nil {
					if callCtx.Err() != nil {
						return
					}
					result <- fault.ServerError(http.StatusServiceUnavailable, "workflow_owner_lost", "The workflow step owner could not be verified.")
					cancel()
					return
				}
			}
		}
	}()
	return callCtx, func() *fault.Error {
		cancel()
		<-done
		select {
		case problem := <-result:
			return problem
		default:
		}
		select {
		case <-debit.leaseStarted:
			checkCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer stop()
			if err := ledger.RenewWorkflowModelLease(checkCtx, debit.id, debit.claim); err != nil {
				return fault.ServerError(http.StatusServiceUnavailable, "workflow_owner_lost", "The workflow step owner could not be verified.")
			}
		default:
		}
		return nil
	}
}

func canonicalSessionID(id string) string {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id {
		return ""
	}
	return id
}

func newSessionDebitID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func (s *Server) sessionExposureHeld(ctx context.Context, keyID string) (bool, *fault.Error) {
	if s.webrtcExposureHeld(keyID) {
		return true, nil
	}
	if s.deps.Debits == nil {
		return false, nil
	}
	held, err := s.deps.Debits.HasUnknownDebit(ctx, keyID, max(s.cfg.WebRTCBound, nativeDuplexSession))
	if err != nil {
		return false, fault.ServerError(http.StatusInternalServerError, "debit_ledger_unavailable", "The durable debit ledger is unavailable, so unresolved session exposure cannot be checked.")
	}
	return held, nil
}

func (s *Server) reserveSessionDebit(ctx context.Context, x *call, authority keyauthority.Authority, attempt routing.Attempt, supply []cappedOwner) *fault.Error {
	if e := x.workflowModelCost(attempt); e != nil {
		return e
	}
	if s.deps.Debits == nil || x.callerCostExempt() && x.workflow == nil {
		return nil
	}
	snapshot := x.snapshot()
	if snapshot == nil || x.route == nil {
		return fault.ServerError(http.StatusInternalServerError, "debit_ledger_unavailable", "The session cannot be reserved without a pinned release.")
	}
	releaseID := canonicalSessionID(snapshot.Generation().ID)
	routeRevisionID := canonicalSessionID(x.route.RevisionID)
	if releaseID == "" {
		return fault.ServerError(http.StatusInternalServerError, "debit_ledger_unavailable", "The session cannot be reserved without a pinned release.")
	}
	revisionID := routeRevisionID
	if revisionID == "" {
		revisionID = releaseID
	}
	currency := ""
	if attempt.Price != nil {
		currency = attempt.Price.Currency
	}
	if currency == "" {
		resolved, err := s.deps.Debits.Currency(ctx)
		if err != nil || resolved == "" {
			return limitsUnavailable()
		}
		currency = resolved
	}
	bound := x.attemptCostBound(attempt)
	charge := x.attemptChargeBound(attempt, bound)
	dispatchID, err := newSessionDebitID()
	if err != nil {
		return limitsUnavailable()
	}
	claimID, err := newSessionDebitID()
	if err != nil {
		return limitsUnavailable()
	}
	projectID := ""
	if authority.ProjectID != nil {
		projectID = *authority.ProjectID
	}
	debit := &sessionDebit{id: dispatchID, claim: claimID, targetID: canonicalSessionID(attempt.TargetID), routeRevisionID: routeRevisionID, leaseStarted: make(chan struct{})}
	intent := debits.Intent{
		ID: dispatchID, APIKeyID: authority.ID, ProjectID: projectID, Kind: debits.KindAPI, ClaimID: claimID,
		Currency: currency, CostBound: bound.String(), ChargeBound: charge.String(),
		PreparedUntil: time.Now().Add(90 * time.Second),
		Affinity: debits.Affinity{
			ReleaseID: releaseID, RevisionID: revisionID, RouteRevisionID: routeRevisionID,
			ProviderID: canonicalSessionID(attempt.ProviderID), ProviderRevisionID: canonicalSessionID(attempt.ProviderRevisionID),
			TargetID: debit.targetID,
		},
	}
	if x.workflow != nil {
		stepID, err := newSessionDebitID()
		if err != nil {
			return limitsUnavailable()
		}
		debit.runID, debit.stepID = x.workflow.runID, stepID
		intent.Kind, intent.RunID, intent.StepID = debits.KindModel, debit.runID, debit.stepID
	}
	boundaries := x.admissionAuthority(authority).CostBoundaries(x.limitRoute())
	if !boundaries.Resolved {
		return limitsUnavailable()
	}
	capped := slices.DeleteFunc(slices.Clone(supply), func(owner cappedOwner) bool { return owner.limits == nil })
	owners, admissions := boundaries.Owners(), boundaries.Admissions()
	for _, owner := range capped {
		cap := capRequest(owner, time.Hour).Cost
		owners = append(owners, cap.Owner)
		admissions = append(admissions, debits.Boundary{Owner: cap.Owner, Budget: cap})
	}
	var state debits.State
	if x.workflow != nil {
		state, err = s.deps.WorkflowRuns.Prepare(ctx, authority, intent, x.workflow.ordinal, owners)
	} else {
		state, err = s.deps.Debits.Prepare(ctx, intent, owners)
	}
	if err != nil || state != debits.Prepared {
		return limitsUnavailable()
	}
	if x.workflow != nil {
		x.workflow.ordinal++
	}
	if err := s.deps.Debits.Admit(ctx, dispatchID, claimID, admissions); err != nil {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webrtcCloseTimeout)
		_ = s.deps.Debits.ReleaseUndispatched(releaseCtx, dispatchID, claimID)
		cancel()
		if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
			if s.deps.Admission != nil {
				s.deps.Admission.recordRejection(exceeded.Dimension)
			}
			return rateLimitedBasis(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate, exceeded.Basis)
		}
		return limitsUnavailable()
	}
	x.sessionDebit = debit
	return nil
}

func (s *Server) markSessionDispatched(ctx context.Context, x *call) *fault.Error {
	if x.sessionDebit == nil || s.deps.Debits == nil {
		return nil
	}
	err := func() error {
		if x.workflow == nil {
			return s.deps.Debits.MarkDispatched(ctx, x.sessionDebit.id, x.sessionDebit.claim)
		}
		ledger, ok := s.deps.Debits.(workflowModelLeases)
		if !ok {
			return debits.ErrAuthority
		}
		return ledger.MarkWorkflowModelDispatched(ctx, x.sessionDebit.id, x.sessionDebit.claim)
	}()
	if err != nil {
		return fault.ServerError(http.StatusInternalServerError, "debit_ledger_unavailable", "The durable debit could not be dispatched, so the provider session was not created.")
	}
	x.sessionDebit.dispatched = true
	if x.workflow != nil {
		close(x.sessionDebit.leaseStarted)
	}
	return nil
}

func (s *Server) abandonSessionDebit(ctx context.Context, x *call) {
	if x.sessionDebit == nil || x.sessionDebit.dispatched || s.deps.Debits == nil {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webrtcCloseTimeout)
	if err := s.deps.Debits.ReleaseUndispatched(releaseCtx, x.sessionDebit.id, x.sessionDebit.claim); err != nil && s.log != nil {
		s.log.Warn("session debit release failed", "error", err.Error())
	}
	cancel()
	for i := range x.facts {
		x.facts[i].Execution = nil
	}
	x.sessionDebit = nil
}

func (s *Server) markSessionUnknown(ctx context.Context, x *call) {
	if x.sessionDebit == nil || !x.sessionDebit.dispatched || s.deps.Debits == nil {
		return
	}
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webrtcCloseTimeout)
	if err := s.deps.Debits.MarkUnknown(markCtx, x.sessionDebit.id, x.sessionDebit.claim); err != nil && s.log != nil {
		s.log.Warn("session debit exposure could not be marked unknown", "error", err.Error())
	}
	cancel()
}

func (s *Server) finishSessionDebit(ctx context.Context, x *call, failed bool) {
	if x.sessionDebit == nil {
		return
	}
	if !x.sessionDebit.dispatched {
		s.abandonSessionDebit(ctx, x)
		return
	}
	if failed {
		s.markSessionUnknown(ctx, x)
	}
}
