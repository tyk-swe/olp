package gateway

import (
	"context"
	"fmt"
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
)

type workflowDebitSettler interface {
	SettleWorkflow(context.Context, *usage.Event, int) ([]limits.Snapshot, error)
}

func (d durableSessionDebits) SettleWorkflow(ctx context.Context, event *usage.Event, attempt int) ([]limits.Snapshot, error) {
	return ingest.PersistWorkflowAttempt(ctx, d.pool, event, attempt)
}

func (s *Server) settleWorkflowModelAttempt(ctx context.Context, x *call, fact events.AttemptFact) *fault.Error {
	settler, ok := s.deps.Debits.(workflowDebitSettler)
	if !ok {
		return s.workflowSettlementFailure(ctx, x, fmt.Errorf("workflow settlement is unavailable"))
	}
	if x.request.release == nil || x.request.release.Snapshot() == nil {
		return s.workflowSettlementFailure(ctx, x, fmt.Errorf("workflow release is unavailable"))
	}
	completedAt := s.now()
	envelope := events.Envelope{
		RequestID: x.request.id, AccountingID: x.request.accountingID(), ClientIP: x.request.clientIP,
		Actor: x.actor, KeyID: x.keyID, EndUserDigest: x.endUserDigest, BudgetGroupID: x.budgetGroupID,
		AgentPrincipalID: x.authority.DelegationAgentID, MemberPrincipalID: x.authority.DelegationMemberID, DelegationPairDigest: x.authority.DelegationPairDigest,
		Attribution: x.attribution, UserID: x.userID, Family: string(x.family), Mode: x.mode,
		Operation: x.operationName(), Surface: x.surfaceName(), Route: x.named().Slug,
		RouteRevisionID: x.named().RevisionID, Status: fact.Status, StartedAt: x.request.startedAt,
		CompletedAt: completedAt, Duration: completedAt.Sub(x.request.startedAt), Attempts: x.facts,
		RuntimeGenerationID: x.request.release.Snapshot().Generation().ID, ReleaseSequence: x.request.release.Sequence,
	}
	event, err := events.UsageEvent(envelope)
	if err != nil {
		return s.workflowSettlementFailure(ctx, x, err)
	}
	if event == nil {
		return s.workflowSettlementFailure(ctx, x, fmt.Errorf("workflow model attempt lacks accounting identity"))
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), events.PersistTimeout)
	defer cancel()
	snapshots, settleErr := settler.SettleWorkflow(settleCtx, event, len(event.Attempts)-1)
	if s.deps.Admission != nil && s.deps.Admission.limiter != nil {
		if err := s.deps.Admission.limiter.InstallSnapshots(settleCtx, snapshots); err != nil {
			return s.workflowSettlementFailure(ctx, x, err)
		}
	}
	if settleErr != nil {
		return s.workflowSettlementFailure(ctx, x, settleErr)
	}
	return nil
}

func (s *Server) workflowSettlementFailure(ctx context.Context, x *call, err error) *fault.Error {
	s.markSessionUnknown(ctx, x)
	if s.log != nil {
		s.log.Warn("workflow model attempt settlement failed", "error", err.Error())
	}
	return fault.ServerError(http.StatusServiceUnavailable, "debit_ledger_unavailable", "The workflow model attempt could not be settled.")
}
