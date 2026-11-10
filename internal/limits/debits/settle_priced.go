package debits

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/ledger"
)

func (r Result) pricedOutcome() bool {
	return r.Final && r.Complete && validAmount(r.Cost) && validAmount(r.Charge) && len(r.ResolvesResultDigests) == 0
}

func (h settleHead) admits(r Result) bool {
	return limits.WithinBound(h.costBound, h.observedCost, r.Cost) && limits.WithinBound(h.chargeBound, h.observedCharge, r.Charge)
}

func (r Result) delta() ledger.Delta {
	at := r.ObservedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if r.Unpriced {
		return ledger.Delta{ObservedAt: at, Cost: r.Cost, Charge: r.Charge, Unpriced: 1, Uncharged: 1}
	}
	return ledger.Delta{ObservedAt: at, Cost: r.Cost, Charge: r.Charge}
}

func persistSpendTx(ctx context.Context, tx pgx.Tx, r Result, owners []limits.Owner) error {
	delta := r.delta()
	distinct := slices.CompactFunc(slices.Clone(owners), func(a, b limits.Owner) bool { return a.ID == b.ID })
	for _, owner := range distinct {
		if !owner.Kind.Valid() {
			return ErrConflict
		}
		if _, err := ledger.AddDelta(ctx, tx, owner.Kind, owner.ID, delta); err != nil {
			return err
		}
	}
	return nil
}

func SettleToolTx(ctx context.Context, tx pgx.Tx, r Result) error {
	return settlePricedTx(ctx, tx, r, KindTool)
}

func SettleAgentTx(ctx context.Context, tx pgx.Tx, r Result) error {
	return settlePricedTx(ctx, tx, r, KindAgent)
}

func settlePricedTx(ctx context.Context, tx pgx.Tx, r Result, kind Kind) error {
	if !r.pricedOutcome() {
		return ErrConflict
	}
	head, duplicate, err := beginSettleTx(ctx, tx, r)
	if err != nil || duplicate {
		return err
	}
	if head.kind != kind || head.state != Dispatched && head.state != Unknown {
		return ErrState
	}
	if !head.admits(r) {
		return ErrConflict
	}
	resolution, err := resolutionDigest(nil)
	if err != nil {
		return err
	}
	owners, err := ownersTx(ctx, tx, r.DispatchID)
	if err != nil {
		return err
	}
	if err := appendResultTx(ctx, tx, r, owners, resolution); err != nil {
		return err
	}
	if err := persistSpendTx(ctx, tx, r, owners); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.execution_debits SET observed_cost=observed_cost+$2::numeric,observed_charge=observed_charge+$3::numeric,state='settled',closed_at=clock_timestamp() WHERE id=$1`, r.DispatchID, r.Cost, r.Charge); err != nil {
		return err
	}
	return bumpOwners(ctx, tx, owners)
}
