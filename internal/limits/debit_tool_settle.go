package limits

import (
	"context"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

func WithinBound(bound, observed, amount string) bool {
	limit, ok := new(big.Rat).SetString(bound)
	if !ok {
		return false
	}
	total, ok := new(big.Rat).SetString(observed)
	if !ok {
		return false
	}
	added, ok := new(big.Rat).SetString(amount)
	if !ok {
		return false
	}
	return total.Add(total, added).Cmp(limit) <= 0
}

func AmountEqual(a, b string) bool {
	left, okLeft := new(big.Rat).SetString(a)
	right, okRight := new(big.Rat).SetString(b)
	return okLeft && okRight && left.Cmp(right) == 0
}

func validToolDebitResult(r DebitResult) bool {
	return r.Final && r.Complete && validDebitAmount(r.Cost) && validDebitAmount(r.Charge) && len(r.ResolvesResultDigests) == 0
}

func toolSettleWithinBounds(costBound, chargeBound, observedCost, observedCharge string, r DebitResult) bool {
	return WithinBound(costBound, observedCost, r.Cost) && WithinBound(chargeBound, observedCharge, r.Charge)
}

func persistToolSpendTx(ctx context.Context, tx pgx.Tx, r DebitResult, owners []DebitOwner) error {
	at := r.ObservedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var unpriced, uncharged int64
	if r.Unpriced {
		unpriced, uncharged = 1, 1
	}
	seen := map[string]bool{}
	for _, owner := range owners {
		if seen[owner.ID] {
			continue
		}
		seen[owner.ID] = true
		var err error
		switch owner.Kind {
		case "key":
			_, err = AddCostDelta(ctx, tx, owner.ID, at, r.Cost, unpriced, r.Charge, uncharged)
		case "group":
			_, err = AddGroupCostDelta(ctx, tx, owner.ID, at, r.Cost, unpriced, r.Charge, uncharged)
		case "aggregate":
			_, err = addOwnerDelta(ctx, tx, addAggregateCostDeltaSQL, owner.ID, at, r.Cost, unpriced, r.Charge, uncharged)
		case "end_user":
			_, err = addOwnerDelta(ctx, tx, addEndUserCostDeltaSQL, owner.ID, at, r.Cost, unpriced, r.Charge, uncharged)
		case "supply":
			_, err = AddSupplyCostDelta(ctx, tx, owner.ID, at, r.Cost, unpriced)
		default:
			return ErrDebitConflict
		}
		if err != nil {
			return err
		}
	}
	return nil
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
	if head.kind != "tool" || head.state != DebitDispatched && head.state != DebitUnknown {
		return ErrDebitState
	}
	if !toolSettleWithinBounds(head.costBound, head.chargeBound, head.observedCost, head.observedCharge, r) {
		return ErrDebitConflict
	}
	resolutionDigest, _ := debitResolutionDigest(nil)
	owners, err := debitOwnersTx(ctx, tx, r.DispatchID)
	if err != nil {
		return err
	}
	if err = appendDebitResultTx(ctx, tx, r, owners, resolutionDigest); err != nil {
		return err
	}
	if err = persistToolSpendTx(ctx, tx, r, owners); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE olp.execution_debits SET observed_cost=observed_cost+$2::numeric,observed_charge=observed_charge+$3::numeric,state='settled',closed_at=clock_timestamp() WHERE id=$1`, r.DispatchID, r.Cost, r.Charge); err != nil {
		return err
	}
	return bumpDebitOwners(ctx, tx, owners)
}
