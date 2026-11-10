package session

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/code"
)

func recoveryFrom(recovery *code.Recovery) *string {
	if recovery == nil {
		return nil
	}
	return &recovery.From
}

func recoveryExclusion(recovery *code.Recovery) *string {
	if recovery == nil {
		return nil
	}
	if _, refresh := recovery.CredentialRefresh(); refresh {
		return nil
	}
	return &recovery.From
}

type recoveryOwner struct {
	root      string
	account   string
	principal string
}

func recoverySource(ctx context.Context, tx pgx.Tx, in Admission) (recoveryOwner, error) {
	if in.Recovery == nil || !in.InitialRecovery || in.Route.InitialRecoveryAttempts < 1 || in.Operation.Identity.Parent != "" || in.PreviousResponse != "" {
		return recoveryOwner{}, code.Refuse(409, "code_recovery_unsafe")
	}
	var root, account, principal string
	var safe bool
	err := tx.QueryRow(ctx, `SELECT b.id::text,a.account_id::text,b.principal,
		olp.code_initial_recovery_safe(b.id) AND a.initial_recovery AND a.nonexecution
		AND a.state='settled' AND a.reported_tokens=0 AND a.account_id=b.account_id
		AND a.model=(SELECT coalesce(target_model,model) FROM olp.code_pins WHERE root_id=b.id AND model=$5)
		AND a.operation=$6 AND a.route_revision_id=$7
		AND NOT EXISTS(SELECT 1 FROM olp.code_attempts WHERE recovery_from=a.id)
		AND (SELECT count(*) FROM olp.code_recovery_chain(a.id))=$8 AND $8 BETWEEN 1 AND $9
		FROM olp.code_attempts a JOIN olp.code_bindings b ON b.id=a.binding_id
		WHERE a.id=$1 AND b.route_id=$2 AND b.api_key_id=$3 AND b.conversation=$4`,
		in.Recovery.From, in.Route.ID, in.APIKeyID, in.Operation.Identity.Conversation, in.Operation.PublicModel(), in.Operation.Name, in.Route.RevisionID, in.Recovery.Number, in.Route.InitialRecoveryAttempts).Scan(&root, &account, &principal, &safe)
	if err != nil || !safe {
		return recoveryOwner{}, code.Refuse(409, "code_recovery_unsafe")
	}
	return recoveryOwner{root: root, account: account, principal: principal}, nil
}

func recoverBinding(ctx context.Context, tx pgx.Tx, in Admission) (*selectionResult, error) {
	source, err := recoverySource(ctx, tx, in)
	if err != nil {
		return nil, err
	}
	root, account, principal := source.root, source.account, source.principal
	if refresh, ok := in.Recovery.CredentialRefresh(); ok {
		if refresh.AccountID != account || refresh.GrantGeneration < 1 {
			return nil, code.Refuse(409, "code_recovery_unsafe")
		}
		var current bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_accounts a JOIN olp.provider_grants g ON g.credential_id=a.credential_id WHERE a.id=$1 AND a.principal=$2 AND a.credential_id=$3 AND g.generation>$4 AND g.lapsed_at IS NULL)`,
			account, principal, refresh.CredentialID, refresh.GrantGeneration).Scan(&current); err != nil || !current {
			return nil, code.Refuse(409, "code_recovery_unsafe")
		}
		updated, err := tx.Exec(ctx, `UPDATE olp.code_accounts SET health='healthy',unavailable_until=NULL WHERE id=$1 AND health='unavailable' AND unavailable_until<=$2`, account, refresh.RejectedUntil)
		if err != nil {
			return nil, err
		}
		if updated.RowsAffected() != 1 {
			return nil, code.Refuse(503, "code_account_unavailable")
		}
		return nil, nil
	}
	pins, err := loadPins(ctx, tx, root)
	if err != nil {
		return nil, err
	}
	operations, err := in.Route.RecoveryModels(in.Operation, pins)
	if err != nil {
		return nil, err
	}
	operation, selection, err := recoveryDestination(ctx, tx, in, operations)
	if err != nil {
		return nil, err
	}
	destination := selection.account
	if destination.ID == account {
		return nil, code.Refuse(409, "code_recovery_unsafe")
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.code_bindings SET account_id=$2,principal=$3 WHERE id=$1`, root, destination.ID, destination.Principal); err != nil {
		return nil, err
	}
	adapter, err := revisionAdapter(ctx, tx, in.Route.RevisionID, destination.ProviderID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.code_pins SET account_id=$2,principal=$3,target_model=$4,adapter=$5 WHERE root_id=$1`,
		root, destination.ID, destination.Principal, operation.Model, adapter); err != nil {
		return nil, err
	}
	return &selection, nil
}

func (s *Store) ConfirmNonexecution(ctx context.Context, attempt string, status int) error {
	if status != 401 && status != 429 {
		return code.Refuse(409, "code_recovery_unsafe")
	}
	command, err := s.pool.Exec(ctx, `UPDATE olp.code_attempts SET nonexecution=true
		WHERE id=$1 AND state='uncertain' AND outcome_origin='upstream' AND outcome='rejected' AND upstream_status=$2`, attempt, status)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return code.Refuse(409, "code_recovery_unsafe")
	}
	return nil
}

func (s *Store) ObserveCooldown(ctx context.Context, account string, until time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE olp.code_accounts SET unavailable_until=greatest(unavailable_until,$2) WHERE id=$1`, account, until)
	return err
}

func recoveryDestination(ctx context.Context, tx pgx.Tx, in Admission, operations []code.Operation) (code.Operation, selectionResult, error) {
	var retryAts []*time.Time
	for _, operation := range operations {
		selected := in.withOperation(operation)
		var authority keyauthority.Authority
		if _, err := loadAuthority(ctx, tx, selected, &authority, scopeInference); err != nil {
			return code.Operation{}, selectionResult{}, err
		}
		selection, err := selectAccount(ctx, tx, selected, modeInference, "", "", nil, selected.Route.AllowanceOrdering)
		if err == nil {
			return operation, selection, nil
		}
		var refusal *code.Refusal
		if !errors.As(err, &refusal) || refusal.Code != "code_account_unavailable" {
			return code.Operation{}, selectionResult{}, err
		}
		retryAts = append(retryAts, refusal.RetryAt)
	}
	return code.Operation{}, selectionResult{}, &code.Refusal{Status: 503, Code: "code_account_unavailable", RetryAt: code.EarliestRecovery(time.Now().UTC(), retryAts...)}
}
