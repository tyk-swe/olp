package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
)

type workflowRecoveryLedger interface {
	SettleRecovery(context.Context, limits.DebitResult) error
	PreviousRecovery(context.Context, string, string, []string) (bool, bool, error)
}

type workflowRecoveryBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type workflowRecoveryResult struct {
	Content []workflowRecoveryBlock `json:"content"`
}

func (s *Server) recoverWorkflowContent(ctx context.Context, x *call, authority access.Authority, callID string, arguments json.RawMessage) (json.RawMessage, error) {
	surface, err := guardrails.NativeSurface(x.generation.parsed.OIF().Document(), false)
	if err != nil || callID == "" || slices.Contains(x.workflowRecoveryCalls, callID) || slices.ContainsFunc(surface.Segments, func(segment guardrails.Segment) bool {
		return segment.Kind == "tool_result" && segment.ToolCallID == callID
	}) {
		return nil, &mcp.WorkflowError{Status: 400, Code: "invalid_tool_call", Message: "The recovery call identifier is invalid."}
	}
	var request struct {
		ID *int `json:"id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.ID == nil || *request.ID < 0 || *request.ID >= len(x.workflowRecovery) {
		return nil, &mcp.WorkflowError{Status: 400, Code: "invalid_tool_call", Message: "The recovery identifier is invalid."}
	}
	if err := decoder.Decode(new(struct{})); err != io.EOF {
		return nil, &mcp.WorkflowError{Status: 400, Code: "invalid_tool_call", Message: "The recovery identifier is invalid."}
	}
	ledger, ok := s.Debits.(workflowRecoveryLedger)
	if !ok || authority.ProjectID == nil || x.route == nil || x.snapshot() == nil {
		return nil, &mcp.WorkflowError{Status: 503, Code: "tool_execution_unavailable", Message: "Recovery accounting is unavailable."}
	}
	runID, err := uuid.Parse(x.workflow.runID)
	if err != nil {
		return nil, err
	}
	recoveryID := func(ordinal int) string {
		return uuid.NewSHA1(runID, []byte("olp-recovery:"+strconv.Itoa(ordinal)+":"+callID)).String()
	}
	previousIDs := slices.Collect(func(yield func(string) bool) {
		for ordinal := 0; ordinal < x.workflow.ordinal; ordinal++ {
			if !yield(recoveryID(ordinal)) {
				return
			}
		}
	})
	settled, pending, err := ledger.PreviousRecovery(ctx, x.workflow.runID, authority.ID, previousIDs)
	if err != nil || pending {
		return nil, &mcp.WorkflowError{Status: 503, Code: "tool_execution_unavailable", Message: "Recovery accounting is unavailable."}
	}
	if settled {
		x.workflowRecoveryCalls = append(x.workflowRecoveryCalls, callID)
		return json.Marshal(workflowRecoveryResult{Content: []workflowRecoveryBlock{{Type: "text", Text: x.workflowRecovery[*request.ID].Text}}})
	}
	dispatchID := recoveryID(x.workflow.ordinal)
	claimID, err := newSessionDebitID()
	if err != nil {
		return nil, err
	}
	stepID, err := newSessionDebitID()
	if err != nil {
		return nil, err
	}
	currency, err := s.Debits.Currency(ctx)
	if err != nil {
		return nil, err
	}
	intent := limits.DebitIntent{
		ID: dispatchID, APIKeyID: authority.ID, ProjectID: *authority.ProjectID,
		RunID: x.workflow.runID, StepID: stepID, Kind: "tool", ClaimID: claimID,
		Currency: currency, CostBound: "0", ChargeBound: "0", PreparedUntil: time.Now().Add(90 * time.Second),
		Affinity: limits.ExecutionAffinity{ReleaseID: canonicalSessionID(x.snapshot().Generation.ID), RevisionID: canonicalSessionID(x.route.RevisionID), RouteRevisionID: canonicalSessionID(x.route.RevisionID)},
	}
	owners, requestLimits, valid := sessionDebitBoundaries(x, authority)
	if !valid {
		return nil, &mcp.WorkflowError{Status: 503, Code: "tool_execution_unavailable", Message: "Recovery accounting is unavailable."}
	}
	state, err := s.WorkflowRuns.Prepare(ctx, authority, intent, x.workflow.ordinal, owners)
	if err != nil || state != limits.DebitPrepared {
		return nil, &mcp.WorkflowError{Status: 503, Code: "tool_execution_unavailable", Message: "Recovery accounting is unavailable."}
	}
	if err := s.Debits.Admit(ctx, dispatchID, claimID, requestLimits); err != nil {
		_ = s.Debits.ReleaseUndispatched(context.WithoutCancel(ctx), dispatchID, claimID)
		return nil, err
	}
	if err := s.Debits.MarkDispatched(ctx, dispatchID, claimID); err != nil {
		return nil, err
	}
	eventID, err := newSessionDebitID()
	if err != nil {
		_ = s.Debits.MarkUnknown(context.WithoutCancel(ctx), dispatchID, claimID)
		return nil, err
	}
	resultDigest := sha256.Sum256([]byte("recovery-result:" + dispatchID))
	evidenceDigest := sha256.Sum256([]byte("recovery-evidence:" + dispatchID))
	result := limits.DebitResult{DispatchID: dispatchID, EventID: eventID, ResultDigest: resultDigest[:], EvidenceDigest: evidenceDigest[:], Cost: "0", Charge: "0", Final: true, Complete: true}
	if err := ledger.SettleRecovery(ctx, result); err != nil {
		_ = s.Debits.MarkUnknown(context.WithoutCancel(ctx), dispatchID, claimID)
		return nil, err
	}
	x.workflowRecoveryCalls = append(x.workflowRecoveryCalls, callID)
	return json.Marshal(workflowRecoveryResult{Content: []workflowRecoveryBlock{{Type: "text", Text: x.workflowRecovery[*request.ID].Text}}})
}
