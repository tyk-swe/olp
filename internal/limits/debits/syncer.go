package debits

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/ledger"
	"github.com/tyk-swe/olp/internal/limits/scripts"
)

const (
	versionCacheSize = 8192
	reconcileBatch   = 128
	firstOwner       = "00000000-0000-0000-0000-000000000000"
)

type versionCache struct {
	mu   sync.Mutex
	seen map[string]int64
}

func (c *versionCache) current(owner string, version int64) bool {
	if version == 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen[owner] == version
}

func (c *versionCache) record(owner string, version int64) {
	if version == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) >= versionCacheSize {
		clear(c.seen)
	}
	c.seen[owner] = version
}

type Syncer struct {
	store    *Store
	scripts  scripts.Runner
	versions versionCache

	reconcileMu    sync.Mutex
	reconcileAfter string
}

func NewSyncer(store *Store, runner scripts.Runner) *Syncer {
	return &Syncer{store: store, scripts: runner, versions: versionCache{seen: map[string]int64{}}, reconcileAfter: firstOwner}
}

func (y *Syncer) SyncOwner(ctx context.Context, owner string) error {
	if !canonicalID(owner) {
		return ErrAuthority
	}
	return y.store.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockOwners(ctx, tx, []limits.Owner{{ID: owner}}); err != nil {
			return err
		}
		return y.syncOwnerTx(ctx, tx, owner)
	})
}

func (y *Syncer) syncOwnerTx(ctx context.Context, tx pgx.Tx, owner string) error {
	w, err := ledger.CurrentBudgetWindows(ctx, tx, time.Now())
	if err != nil {
		return err
	}
	var version int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM olp.execution_debit_versions WHERE owner_id=$1),0)`, owner).Scan(&version); err != nil {
		return err
	}
	for _, basis := range [...]limits.Basis{limits.BasisCost, limits.BasisCharge} {
		durable := scripts.Durable{Version: version}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(GREATEST(0,o.bound-CASE WHEN o.basis='charge' THEN d.observed_charge ELSE d.observed_cost END)),0)::text,COALESCE(bool_or(d.bound_violation OR d.state='conflict'),false)
 FROM olp.execution_debit_owners o JOIN olp.execution_debits d ON d.id=o.dispatch_id
 WHERE o.owner_id=$1 AND o.basis=$2 AND d.closed_at IS NULL`, owner, basis).Scan(&durable.Open, &durable.Blocked); err != nil {
			return err
		}
		snapshot, err := spendTx(ctx, tx, limits.Owner{ID: owner, Basis: basis}, w)
		if err != nil {
			return err
		}
		if _, err := y.scripts.InstallSnapshot(ctx, snapshot, &durable); err != nil {
			return err
		}
	}
	y.versions.record(owner, version)
	return nil
}

type windowSpend struct {
	kind, total string
	missing     int64
	complete    bool
}

func spendTx(ctx context.Context, tx pgx.Tx, owner limits.Owner, w limits.Windows) (limits.Snapshot, error) {
	rows, err := tx.Query(ctx, `SELECT window_kind,sum(CASE WHEN $2='charge' THEN charged ELSE accrued END)::text,
 sum(CASE WHEN $2='charge' THEN uncharged_attempts ELSE unpriced_attempts END)::bigint,bool_and(weekly_complete)
 FROM olp.execution_owner_spend WHERE owner_id=$1 AND ((window_kind='day' AND window_id=$3) OR (window_kind='month' AND window_id=$4) OR (window_kind='week' AND window_id=$5)) GROUP BY window_kind`, owner.ID, string(owner.Basis), w.DailyID, w.MonthlyID, w.WeeklyID)
	if err != nil {
		return limits.Snapshot{}, err
	}
	spent, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (windowSpend, error) {
		var s windowSpend
		err := row.Scan(&s.kind, &s.total, &s.missing, &s.complete)
		return s, err
	})
	if err != nil {
		return limits.Snapshot{}, err
	}
	by := make(map[string]windowSpend, len(spent))
	for _, s := range spent {
		by[s.kind] = s
	}
	day, month, week := by["day"], by["month"], by["week"]
	if day.total == "" || month.total == "" {
		return limits.Snapshot{}, limits.ErrUninitializedCost
	}
	snapshot := limits.Snapshot{OwnerID: owner.ID, Basis: owner.Basis, Windows: w, DailyAccrued: day.total, MonthlyAccrued: month.total, UnpricedAttempts: month.missing}
	if week.complete {
		snapshot.WeeklyAccrued = week.total
	}
	return snapshot, nil
}

func (y *Syncer) CheckOwnerAccounting(ctx context.Context, owner string) error {
	var pending bool
	var version int64
	if err := y.store.pool.QueryRow(ctx, `SELECT olp.has_pending_resource_usage($1::uuid),COALESCE((SELECT version FROM olp.execution_debit_versions WHERE owner_id=$1::uuid),0)`, owner).Scan(&pending, &version); err != nil {
		return err
	}
	if pending {
		return limits.ErrUninitializedCost
	}
	if y.versions.current(owner, version) {
		return nil
	}
	return y.SyncOwner(ctx, owner)
}

type Boundary struct {
	Owner  limits.Owner
	Budget *admission.CostBudget
}

func (y *Syncer) Admit(ctx context.Context, id, claim string, boundaries []Boundary) error {
	return y.store.inTx(ctx, func(tx pgx.Tx) error {
		var state State
		if err := tx.QueryRow(ctx, `SELECT state FROM olp.execution_debits WHERE id=$1 AND claim_id=$2 AND prepared_until>clock_timestamp() FOR UPDATE`, id, claim).Scan(&state); err != nil || state != Prepared {
			return ErrState
		}
		owners, err := ownersTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if len(boundaries)*2 != len(owners) {
			return ErrConflict
		}
		if err := lockOwners(ctx, tx, owners); err != nil {
			return err
		}
		kinds := make(map[limits.Owner]limits.OwnerKind, len(owners))
		for _, o := range owners {
			kinds[limits.Owner{ID: o.ID, Basis: o.Basis}] = o.Kind
		}
		seen := make(map[string]bool, len(boundaries))
		for _, b := range boundaries {
			kind, owned := kinds[limits.Owner{ID: b.Owner.ID, Basis: b.Owner.Basis.Normalized()}]
			if seen[b.Owner.ID] || !owned {
				return ErrConflict
			}
			seen[b.Owner.ID] = true
			if b.Budget == nil {
				continue
			}
			if err := y.admitTx(ctx, tx, limits.Owner{ID: b.Owner.ID, Kind: kind, Basis: b.Owner.Basis}, *b.Budget); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE olp.execution_debits SET state='ready' WHERE id=$1`, id)
		return err
	})
}

func (y *Syncer) admitTx(ctx context.Context, tx pgx.Tx, owner limits.Owner, budget admission.CostBudget) error {
	if budget.Owner.ID != owner.ID || budget.Owner.Basis.Normalized() != owner.Basis.Normalized() || !canonicalID(owner.ID) || budget.Validate() != nil {
		return ErrConflict
	}
	if err := y.syncFreshTx(ctx, tx, owner); err != nil {
		return err
	}
	return y.scripts.VerifyCost(ctx, budget.Caps(true))
}

func (y *Syncer) syncFreshTx(ctx context.Context, tx pgx.Tx, owner limits.Owner) error {
	err := y.syncOwnerTx(ctx, tx, owner.ID)
	if !errors.Is(err, limits.ErrUninitializedCost) {
		return err
	}
	if err := ledger.OpenFreshOwner(ctx, tx, owner.Kind, owner.ID); err != nil {
		return err
	}
	return y.syncOwnerTx(ctx, tx, owner.ID)
}

func (y *Syncer) ReconcileDebits(ctx context.Context) error {
	y.reconcileMu.Lock()
	defer y.reconcileMu.Unlock()
	if _, err := y.store.RecoverPrepared(ctx, 256); err != nil {
		return err
	}
	rows, err := y.store.pool.Query(ctx, `SELECT owner_id::text FROM olp.execution_debit_versions WHERE owner_id>$1::uuid ORDER BY owner_id LIMIT $2`, y.reconcileAfter, reconcileBatch)
	if err != nil {
		return err
	}
	owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, owner := range owners {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := y.SyncOwner(ctx, owner)
		y.reconcileAfter = owner
		if err != nil && !errors.Is(err, limits.ErrUninitializedCost) {
			return err
		}
	}
	if len(owners) < reconcileBatch {
		y.reconcileAfter = firstOwner
	}
	return nil
}
