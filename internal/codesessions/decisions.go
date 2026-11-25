package codesessions

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/domain/ids"
)

func recordCodeDecision(ctx context.Context, tx pgx.Tx, in Admission, binding codemode.Binding, attemptID *string, selection codeSelection, reason codemode.DecisionReason) error {
	chosen, ok := selection.chosen()
	if !ok {
		return fmt.Errorf("code decision chosen account missing from candidates")
	}
	recoveryEnvelope, recoveryOrdinal := func() (*string, *int) {
		if in.Recovery == nil {
			return nil, nil
		}
		if in.Recovery.Envelope == "" {
			return nil, &in.Recovery.Number
		}
		return &in.Recovery.Envelope, &in.Recovery.Number
	}()
	decision := codemode.Decision{
		ID:               ids.New(),
		AttemptID:        attemptID,
		ProjectID:        in.Route.ProjectID,
		RouteID:          in.Route.ID,
		RouteRevisionID:  in.Route.RevisionID,
		APIKeyID:         in.APIKeyID,
		BindingID:        binding.ID,
		RootID:           binding.RootID,
		ChosenAccountID:  selection.account.ID,
		Reason:           reason,
		Skipped:          codemode.SkippedAccounts(selection.candidates, chosen),
		RecoveryEnvelope: recoveryEnvelope,
		RecoveryOrdinal:  recoveryOrdinal,
	}
	if err := decision.Validate(); err != nil {
		return err
	}
	skipped, err := json.Marshal(decision.Skipped)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO olp.code_decisions(id,attempt_id,project_id,route_id,route_revision_id,api_key_id,binding_id,root_id,chosen_account_id,reason,skipped,recovery_envelope,recovery_ordinal)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		decision.ID, decision.AttemptID, decision.ProjectID, decision.RouteID, decision.RouteRevisionID, decision.APIKeyID, decision.BindingID, decision.RootID, decision.ChosenAccountID, decision.Reason, skipped, decision.RecoveryEnvelope, decision.RecoveryOrdinal)
	return err
}
