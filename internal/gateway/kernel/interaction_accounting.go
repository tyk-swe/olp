package kernel

import (
	"context"

	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
)

// Terminal observations belong to the original generation. The resource owner
// serializes competing GETs, cancellations and resumed streams across replicas.
func (s *Server) observeGeminiInteraction(ctx context.Context, owner, localID, status string, object oif.Value) (ingest.Persisted, error) {
	evidence := usage.AttemptUsage{Evidence: usage.EvidenceUnobservedUncertain}
	if observed := geminiInteractionUsage(object); observed != nil {
		evidence = events.ObservedUsage(observed, geminilifecycle.InteractionTurnFinished(status), "generation")
	}
	result, err := s.Deps.Resources.ObserveInteraction(ctx, owner, localID, status, evidence, s.Now())
	if err != nil {
		return ingest.Persisted{}, err
	}
	if err := s.Deps.Admission.InstallSnapshots(ctx, result.CostSnapshots); err != nil {
		s.log.Warn("interaction cost snapshot application failed; reconciliation will repair it", "error_type", "cost_snapshot")
	}
	return result, nil
}
