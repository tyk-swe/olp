package limits

import (
	"context"

	"github.com/jackc/pgx/v5"
)

func SettleToolDebitTx(ctx context.Context, tx pgx.Tx, r DebitResult) error {
	if !r.Final || !r.Complete || !equalDebitAmount(r.Cost, "0") || !equalDebitAmount(r.Charge, "0") || len(r.ResolvesResultDigests) != 0 {
		return ErrDebitConflict
	}
	var state DebitState
	var kind string
	var zero bool
	if err := tx.QueryRow(ctx, `SELECT state,kind,cost_bound=0 AND charge_bound=0 FROM olp.execution_debits WHERE id=$1 FOR UPDATE`, r.DispatchID).Scan(&state, &kind, &zero); err != nil {
		return err
	}
	if duplicate, err := DebitResultReplayTx(ctx, tx, r); err != nil {
		return err
	} else if duplicate {
		return nil
	}
	if kind != "tool" || !zero || state != DebitDispatched && state != DebitUnknown {
		return ErrDebitState
	}
	resolutionDigest, _ := debitResolutionDigest(nil)
	owners, err := debitOwnersTx(ctx, tx, r.DispatchID)
	if err != nil {
		return err
	}
	if err = lockDebitOwners(ctx, tx, owners); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO olp.execution_debit_results(dispatch_id,result_digest,evidence_digest,event_id,cost,charge,final,complete,resolution_digest) VALUES($1,$2,$3,$4,0,0,true,true,$5)`, r.DispatchID, r.ResultDigest, r.EvidenceDigest, r.EventID, resolutionDigest); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE olp.execution_debits SET state='settled',closed_at=clock_timestamp() WHERE id=$1`, r.DispatchID); err != nil {
		return err
	}
	return bumpDebitOwners(ctx, tx, owners)
}
