package limits

import (
	"context"

	"github.com/jackc/pgx/v5"
)

func validToolDebitResult(r DebitResult) bool {
	return r.Final && r.Complete && equalDebitAmount(r.Cost, "0") && equalDebitAmount(r.Charge, "0") && len(r.ResolvesResultDigests) == 0
}

func SettleToolDebitTx(ctx context.Context, tx pgx.Tx, r DebitResult) error {
	if !validToolDebitResult(r) {
		return ErrDebitConflict
	}
	head, duplicate, err := beginDebitSettleTx(ctx, tx, r)
	if err != nil {
		return err
	}
	if duplicate {
		return nil
	}
	if head.kind != "tool" || !head.zeroBound || head.state != DebitDispatched && head.state != DebitUnknown {
		return ErrDebitState
	}
	resolutionDigest, _ := debitResolutionDigest(nil)
	owners, err := debitOwnersTx(ctx, tx, r.DispatchID)
	if err != nil {
		return err
	}
	if err = appendDebitResultTx(ctx, tx, r, owners, resolutionDigest); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE olp.execution_debits SET state='settled',closed_at=clock_timestamp() WHERE id=$1`, r.DispatchID); err != nil {
		return err
	}
	return bumpDebitOwners(ctx, tx, owners)
}
