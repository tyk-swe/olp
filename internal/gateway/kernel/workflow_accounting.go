package kernel

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

func (d durableSessionDebits) SettleWorkflow(ctx context.Context, event *usage.Event, attempt int) ([]limits.Snapshot, error) {
	return ingest.PersistWorkflowAttempt(ctx, d.pool, event, attempt)
}

func (s *Server) settleWorkflowModelAttempt(ctx context.Context, x *Call, fact events.AttemptFact) *fault.Error {
	if x.request.release == nil || x.request.release.Snapshot() == nil {
		return s.workflowSettlementFailure(ctx, x, fmt.Errorf("workflow release is unavailable"))
	}
	completedAt := s.Now()
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
	snapshots, settleErr := s.Deps.Debits.SettleWorkflow(settleCtx, event, len(event.Attempts)-1)
	if err := s.Deps.Admission.InstallSnapshots(settleCtx, snapshots); err != nil {
		return s.workflowSettlementFailure(ctx, x, err)
	}
	if settleErr != nil {
		return s.workflowSettlementFailure(ctx, x, settleErr)
	}
	return nil
}

func (s *Server) workflowSettlementFailure(ctx context.Context, x *Call, err error) *fault.Error {
	s.markSessionUnknown(ctx, x)
	if s.Log != nil {
		s.Log.Warn("workflow model attempt settlement failed", "error", err.Error())
	}
	return fault.ServerError(http.StatusServiceUnavailable, "debit_ledger_unavailable", "The workflow model attempt could not be settled.")
}
