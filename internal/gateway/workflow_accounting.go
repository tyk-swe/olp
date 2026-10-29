package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/usage"
)

type workflowDebitSettler interface {
	SettleWorkflow(context.Context, *usage.Event, int) ([]limits.CostSnapshot, error)
}

func (d durableSessionDebits) SettleWorkflow(ctx context.Context, event *usage.Event, attempt int) ([]limits.CostSnapshot, error) {
	return usage.PersistWorkflowAttempt(ctx, d.store.Pool, event, attempt)
}

func accountingEventID(e Envelope) (uuid.UUID, error) {
	for _, attempt := range e.Attempts {
		if attempt.Execution != nil && attempt.Execution.RunID != "" {
			return uuid.Parse(attempt.Execution.RunID)
		}
	}
	return uuidNewV7()
}

func accountingAttemptID(e Envelope, index int) (uuid.UUID, error) {
	fact := e.Attempts[index]
	if fact.Execution != nil && fact.Execution.RunID != "" {
		return uuid.Parse(fact.Execution.DispatchID)
	}
	for _, attempt := range e.Attempts {
		if attempt.Execution != nil && attempt.Execution.RunID != "" {
			runID, err := uuid.Parse(attempt.Execution.RunID)
			if err != nil {
				return uuid.Nil, err
			}
			return uuid.NewSHA1(runID, []byte(strconv.Itoa(index+1))), nil
		}
	}
	return uuidNewV7()
}

func (s *Server) settleWorkflowModelAttempt(ctx context.Context, x *call, fact AttemptFact) *Error {
	settler, ok := s.Debits.(workflowDebitSettler)
	if !ok {
		return s.workflowSettlementFailure(ctx, x, fmt.Errorf("workflow settlement is unavailable"))
	}
	if x.request.release == nil || x.request.release.Snapshot == nil {
		return s.workflowSettlementFailure(ctx, x, fmt.Errorf("workflow release is unavailable"))
	}
	completedAt := s.now()
	envelope := Envelope{
		RequestID: x.request.id, AccountingID: x.request.accountingID(), ClientIP: x.request.clientIP,
		Actor: x.actor, KeyID: x.keyID, EndUserDigest: x.endUserDigest, BudgetGroupID: x.budgetGroupID,
		AgentPrincipalID: x.authority.DelegationAgentID, MemberPrincipalID: x.authority.DelegationMemberID, DelegationPairDigest: x.authority.DelegationPairDigest,
		Attribution: x.attribution, UserID: x.userID, Family: string(x.family), Mode: x.mode,
		Operation: x.operationName(), Surface: x.surfaceName(), Route: x.named().Slug,
		RouteRevisionID: x.named().RevisionID, Status: fact.Status, StartedAt: x.request.startedAt,
		CompletedAt: completedAt, Duration: completedAt.Sub(x.request.startedAt), Attempts: x.facts,
		RuntimeGenerationID: x.request.release.Snapshot.Generation.ID, ReleaseSequence: x.request.release.Sequence,
	}
	event, err := accountingEvent(envelope)
	if err != nil {
		return s.workflowSettlementFailure(ctx, x, err)
	}
	if event == nil {
		return s.workflowSettlementFailure(ctx, x, fmt.Errorf("workflow model attempt lacks accounting identity"))
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	snapshots, settleErr := settler.SettleWorkflow(settleCtx, event, len(event.Attempts)-1)
	if s.Admission != nil && s.Admission.limiter != nil {
		for _, snapshot := range snapshots {
			if _, _, err := s.Admission.limiter.ApplyCostSnapshot(settleCtx, snapshot); err != nil {
				return s.workflowSettlementFailure(ctx, x, err)
			}
		}
	}
	if settleErr != nil {
		return s.workflowSettlementFailure(ctx, x, settleErr)
	}
	return nil
}

func (s *Server) workflowSettlementFailure(ctx context.Context, x *call, err error) *Error {
	s.markSessionUnknown(ctx, x)
	if s.log != nil {
		s.log.Warn("workflow model attempt settlement failed", "error", err.Error())
	}
	return serverError(http.StatusServiceUnavailable, "debit_ledger_unavailable", "The workflow model attempt could not be settled.")
}
