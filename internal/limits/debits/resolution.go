package debits

import (
	"bytes"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

type NonAcceptanceProof struct {
	DispatchID     string
	ActorID        string
	EvidenceDigest []byte
}

func ResolveNotAcceptedTx(ctx context.Context, tx pgx.Tx, p NonAcceptanceProof) error {
	if !canonicalID(p.DispatchID) || !canonicalID(p.ActorID) || len(p.EvidenceDigest) != 32 {
		return ErrConflict
	}
	var state State
	if err := tx.QueryRow(ctx, `SELECT state FROM olp.execution_debits WHERE id=$1 FOR UPDATE`, p.DispatchID).Scan(&state); err != nil {
		return err
	}
	var actor string
	var evidence []byte
	err := tx.QueryRow(ctx, `SELECT actor_id::text,evidence_digest FROM olp.execution_debit_resolutions WHERE dispatch_id=$1`, p.DispatchID).Scan(&actor, &evidence)
	if err == nil {
		if actor != p.ActorID || !bytes.Equal(evidence, p.EvidenceDigest) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if state != Dispatched && state != Unknown {
		return ErrState
	}
	var clean bool
	if err = tx.QueryRow(ctx, `SELECT observed_cost=0 AND observed_charge=0 AND NOT EXISTS(SELECT 1 FROM olp.execution_debit_results WHERE dispatch_id=$1 AND (cost<>0 OR charge<>0 OR complete)) FROM olp.execution_debits WHERE id=$1`, p.DispatchID).Scan(&clean); err != nil {
		return err
	}
	if !clean {
		return ErrState
	}
	owners, err := ownersTx(ctx, tx, p.DispatchID)
	if err != nil {
		return err
	}
	if err = lockOwners(ctx, tx, owners); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO olp.execution_debit_resolutions(dispatch_id,actor_id,evidence_digest) VALUES($1,$2,$3)`, p.DispatchID, p.ActorID, p.EvidenceDigest); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE olp.execution_debits SET state='not_accepted',closed_at=clock_timestamp() WHERE id=$1`, p.DispatchID); err != nil {
		return err
	}
	return bumpOwners(ctx, tx, owners)
}
