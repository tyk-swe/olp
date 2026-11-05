package resources

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
)

func recordCodeDecision(ctx context.Context, tx pgx.Tx, in CodeAdmission, binding codemode.Binding, attempt codemode.Attempt, selection codeSelection, reason codemode.DecisionReason) error {
	chosen, ok := selection.chosen()
	if !ok {
		return fmt.Errorf("code decision chosen account missing from candidates")
	}
	evidence, observedAt := codemode.FreshestEvidence(selection.account.Allowance)
	decision := codemode.Decision{
		ID:                 access.NewID(),
		AttemptID:          attempt.ID,
		ProjectID:          in.Route.ProjectID,
		RouteID:            in.Route.ID,
		RouteRevisionID:    in.Route.RevisionID,
		APIKeyID:           in.APIKeyID,
		BindingID:          binding.ID,
		RootID:             binding.RootID,
		ChosenAccountID:    selection.account.ID,
		Reason:             reason,
		Evidence:           evidence,
		EvidenceObservedAt: observedAt,
		Skipped:            codemode.SkippedAccounts(selection.candidates, chosen),
	}
	if in.Recovery != nil {
		decision.RecoveryOrdinal = &in.Recovery.Number
		if in.Recovery.Envelope != "" {
			decision.RecoveryEnvelope = &in.Recovery.Envelope
		}
	}
	if err := decision.Validate(); err != nil {
		return err
	}
	skipped, err := json.Marshal(decision.Skipped)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO olp.code_decisions(id,attempt_id,project_id,route_id,route_revision_id,api_key_id,binding_id,root_id,chosen_account_id,reason,band,evidence,evidence_observed_at,skipped,recovery_envelope,recovery_ordinal)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		decision.ID, decision.AttemptID, decision.ProjectID, decision.RouteID, decision.RouteRevisionID, decision.APIKeyID, decision.BindingID, decision.RootID, decision.ChosenAccountID, decision.Reason, decision.Band, decision.Evidence, decision.EvidenceObservedAt, skipped, decision.RecoveryEnvelope, decision.RecoveryOrdinal)
	return err
}
