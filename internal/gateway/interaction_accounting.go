package gateway

import (
	"context"

	"github.com/tyk-swe/olp/internal/geminilifecycle"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/usage"
)

// Terminal observations belong to the original generation. The resource owner
// serializes competing GETs, cancellations and resumed streams across replicas.
func (s *Server) observeGeminiInteraction(ctx context.Context, owner, localID, status string, object oif.Value) (usage.Persisted, error) {
	evidence := usage.AttemptUsage{BillingUncertain: true}
	if observed := geminiInteractionUsage(object); observed != nil {
		evidence = observedAttemptUsage(observed, geminilifecycle.InteractionTurnFinished(status), "generation")
	}
	result, err := s.Resources.ObserveInteraction(ctx, owner, localID, status, evidence, s.now())
	if err != nil {
		return usage.Persisted{}, err
	}
	if s.Admission != nil && s.Admission.limiter != nil {
		for _, snapshot := range result.CostSnapshots {
			if _, _, err := s.Admission.limiter.ApplyCostSnapshot(ctx, snapshot); err != nil {
				s.log.Warn("interaction cost snapshot application failed; reconciliation will repair it", "error_type", "cost_snapshot")
			}
		}
	}
	return result, nil
}
