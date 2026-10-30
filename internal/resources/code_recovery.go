package resources

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

func codeRecoveryFrom(recovery *CodeRecovery) *string {
	if recovery == nil {
		return nil
	}
	return &recovery.From
}

func codeRecoveryExclusion(recovery *CodeRecovery) *string {
	if recovery == nil || recovery.AccountID != "" {
		return nil
	}
	return &recovery.From
}

func recoverCodeBinding(ctx context.Context, tx pgx.Tx, in CodeAdmission) error {
	if in.Recovery == nil || !in.InitialRecovery || in.Route.InitialRecoveryAttempts < 1 || in.Operation.Identity.Parent != "" || in.PreviousResponse != "" {
		return codemode.Refuse(409, "code_recovery_unsafe")
	}
	var root, account, principal string
	var safe bool
	sourceErr := tx.QueryRow(ctx, `SELECT b.id::text,a.account_id::text,b.principal,
		olp.code_initial_recovery_safe(b.id) AND a.initial_recovery AND a.nonexecution
		AND a.state='settled' AND a.reported_tokens=0 AND a.account_id=b.account_id
		AND a.model=(SELECT coalesce(target_model,model) FROM olp.code_pins WHERE root_id=b.id AND model=$5)
		AND a.operation=$6 AND a.route_revision_id=$7
		AND NOT EXISTS(SELECT 1 FROM olp.code_attempts WHERE recovery_from=a.id)
		AND (SELECT count(*) FROM olp.code_recovery_chain(a.id))=$8 AND $8 BETWEEN 1 AND $9
		FROM olp.code_attempts a JOIN olp.code_bindings b ON b.id=a.binding_id
		WHERE a.id=$1 AND b.route_id=$2 AND b.api_key_id=$3 AND b.conversation=$4`,
		in.Recovery.From, in.Route.ID, in.APIKeyID, in.Operation.Identity.Conversation, in.Operation.PublicModel(), in.Operation.Name, in.Route.RevisionID, in.Recovery.Number, in.Route.InitialRecoveryAttempts).Scan(&root, &account, &principal, &safe)
	if sourceErr != nil || !safe {
		return codemode.Refuse(409, "code_recovery_unsafe")
	}
	if in.Recovery.AccountID != "" {
		if in.Recovery.AccountID != account || in.Recovery.GrantGeneration < 1 {
			return codemode.Refuse(409, "code_recovery_unsafe")
		}
		var current bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_accounts a JOIN olp.provider_grants g ON g.credential_id=a.credential_id WHERE a.id=$1 AND a.principal=$2 AND a.credential_id=$3 AND g.generation>$4 AND g.lapsed_at IS NULL)`, account, principal, in.Recovery.CredentialID, in.Recovery.GrantGeneration).Scan(&current); err != nil || !current {
			return codemode.Refuse(409, "code_recovery_unsafe")
		}
		updated, err := tx.Exec(ctx, `UPDATE olp.code_accounts SET health='healthy',unavailable_until=NULL WHERE id=$1 AND health='unavailable' AND unavailable_until<=$2`, account, in.Recovery.RejectedUntil)
		if err != nil {
			return err
		}
		if updated.RowsAffected() != 1 {
			return codemode.Refuse(503, "code_account_unavailable")
		}
		return nil
	}
	pins, pinsErr := codePins(ctx, tx, root)
	if pinsErr != nil {
		return pinsErr
	}
	operation, modelErr := in.Route.ResolvePinnedModel(in.Operation, pins)
	if modelErr != nil {
		return modelErr
	}
	destination, destinationErr := codeAccount(ctx, tx, in.withOperation(operation), "", "", nil)
	if destinationErr != nil {
		return destinationErr
	}
	if destination.ID == account {
		return codemode.Refuse(409, "code_recovery_unsafe")
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.code_bindings SET account_id=$2,principal=$3 WHERE id=$1`, root, destination.ID, destination.Principal); err != nil {
		return err
	}
	_, pinErr := tx.Exec(ctx, `UPDATE olp.code_pins SET account_id=$2,principal=$3,adapter=`+codeadapter.SQL("v.connections->($4::text)")+` FROM olp.code_route_revisions v WHERE root_id=$1 AND v.id=$5`, root, destination.ID, destination.Principal, destination.ProviderID, in.Route.RevisionID)
	return pinErr
}

func (s *CodeStore) ConfirmNonexecution(ctx context.Context, attempt string, status int) error {
	if status != 401 && status != 429 {
		return codemode.Refuse(409, "code_recovery_unsafe")
	}
	command, err := s.Pool.Exec(ctx, `UPDATE olp.code_attempts SET nonexecution=true
		WHERE id=$1 AND state='uncertain' AND outcome_origin='upstream' AND outcome='rejected' AND upstream_status=$2`, attempt, status)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return codemode.Refuse(409, "code_recovery_unsafe")
	}
	return nil
}

func (s *CodeStore) ObserveCooldown(ctx context.Context, account string, until time.Time) error {
	_, err := s.Pool.Exec(ctx, `UPDATE olp.code_accounts SET unavailable_until=greatest(unavailable_until,$2) WHERE id=$1`, account, until)
	return err
}
