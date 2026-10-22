package limits

import (
	"bytes"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// NonAcceptanceProof is reviewed lifecycle evidence, not a caller assertion or
// a timeout. The lifecycle owner authorizes the actor and verifies provider
// evidence before entering this transaction. No execution is initiated here.
type NonAcceptanceProof struct {
	DispatchID     string
	ActorID        string
	EvidenceDigest []byte
}

func ResolveNotAcceptedTx(ctx context.Context, tx pgx.Tx, p NonAcceptanceProof) error {
	if !canonicalID(p.DispatchID) || !canonicalID(p.ActorID) || len(p.EvidenceDigest) != 32 {
		return ErrDebitConflict
	}
	var state DebitState
	if err := tx.QueryRow(ctx, `SELECT state FROM olp.execution_debits WHERE id=$1 FOR UPDATE`, p.DispatchID).Scan(&state); err != nil {
		return err
	}
	var actor string
	var evidence []byte
	err := tx.QueryRow(ctx, `SELECT actor_id::text,evidence_digest FROM olp.execution_debit_resolutions WHERE dispatch_id=$1`, p.DispatchID).Scan(&actor, &evidence)
	if err == nil {
		if actor != p.ActorID || !bytes.Equal(evidence, p.EvidenceDigest) {
			return ErrDebitConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if state != DebitDispatched && state != DebitUnknown {
		return ErrDebitState
	}
	var clean bool
	if err = tx.QueryRow(ctx, `SELECT observed_cost=0 AND observed_charge=0 AND NOT EXISTS(SELECT 1 FROM olp.execution_debit_results WHERE dispatch_id=$1 AND (cost<>0 OR charge<>0 OR complete)) FROM olp.execution_debits WHERE id=$1`, p.DispatchID).Scan(&clean); err != nil {
		return err
	}
	if !clean {
		return ErrDebitState
	}
	owners, err := debitOwnersTx(ctx, tx, p.DispatchID)
	if err != nil {
		return err
	}
	if err = lockDebitOwners(ctx, tx, owners); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO olp.execution_debit_resolutions(dispatch_id,actor_id,evidence_digest) VALUES($1,$2,$3)`, p.DispatchID, p.ActorID, p.EvidenceDigest); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE olp.execution_debits SET state='not_accepted',closed_at=clock_timestamp() WHERE id=$1`, p.DispatchID); err != nil {
		return err
	}
	return bumpDebitOwners(ctx, tx, owners)
}
