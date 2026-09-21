package gateway

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/usage"
)

func backgroundResponseRequested(parsed *openai.Request) bool {
	var background bool
	return json.Unmarshal(parsed.Field("background"), &background) == nil && background
}

// A background response may outlive its POST or stream. Keep a content-free
// accounting template on the resource so polling uses the generation's key,
// attribution, pricing and request identity.
func (s *Server) pendingResponseUsage(x *execution, fact *AttemptFact, metadata map[string]json.RawMessage) bool {
	if !backgroundResponseRequested(x.parsed) {
		return false
	}
	copyFact := *fact
	copyFact.ResponseUsageDeferred = false
	copyFact.Usage = nil
	copyFact.Class = classSuccess
	copyFact.Committed = true
	copyFact.recordEvidence(true)
	facts := append([]AttemptFact{}, x.facts...)
	if len(facts) > 0 && facts[len(facts)-1].Ordinal == fact.Ordinal {
		facts[len(facts)-1] = copyFact
	} else {
		facts = append(facts, copyFact)
	}
	now := s.now()
	ev := accountingEvent(Envelope{
		AccountingID: x.request.accountingID(), KeyID: x.keyID,
		BudgetGroupID: x.budgetGroupID, Attribution: x.attribution,
		PolicyDecisions: x.policyDecisions, Route: x.route.Slug,
		Operation: x.family.Operation(), Surface: x.family.Surface(),
		RuntimeGenerationID: x.request.release.Snapshot.Generation.ID,
		StartedAt:           x.request.startedAt, CompletedAt: now,
		Duration: now.Sub(x.request.startedAt), Status: http.StatusOK,
		Attempts: facts,
	})
	if ev == nil {
		return false
	}
	encoded, err := json.Marshal(ev)
	if err != nil {
		return false
	}
	metadata["pending_usage"] = encoded
	return true
}

func (s *Server) reconcileResponse(ctx context.Context, localID string, body []byte) *Error {
	status, _ := upstreamString(body, "status")
	switch status {
	case "completed", "cancelled", "failed", "incomplete":
	default:
		return nil
	}
	tokens := responseUsage(body)
	if tokens == nil {
		return nil
	}
	evidence := usage.AttemptUsage{Observed: true, Complete: true,
		InputTokens: &tokens.InputTokens, OutputTokens: &tokens.OutputTokens,
		CachedInputTokens: tokens.CachedInputTokens}
	result, err := s.Resources.ReconcileResponseUsage(ctx, localID, evidence, s.now())
	if err != nil {
		return serverError(http.StatusServiceUnavailable, "response_usage_unavailable", "The stored response usage could not be reconciled; retry the request.")
	}
	if s.Admission != nil && s.Admission.limiter != nil {
		for _, snapshot := range result.CostSnapshots {
			if _, _, err := s.Admission.limiter.ApplyCostSnapshot(ctx, snapshot); err != nil {
				s.log.Warn("response cost snapshot application failed; reconciliation will repair it", "error", err)
			}
		}
	}
	return nil
}
