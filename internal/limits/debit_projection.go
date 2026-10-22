package limits

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type debitProjection struct {
	Version int64
	Amount  string
	Blocked bool
}

// UseDurableDebits binds the authoritative ledger before the limiter is shared
// between goroutines. Warm monetary admission checks the existing Valkey
// projection; a missing projection is rebuilt under the ledger owner lock.
func (l *Limiter) UseDurableDebits(pool *pgxpool.Pool) {
	s := &DebitStore{Pool: pool, Limiter: l}
	l.syncDurable = s.SyncOwner
}

func (s *DebitStore) SyncOwner(ctx context.Context, owner string) error {
	if !canonicalID(owner) || s.Limiter == nil {
		return ErrDebitAuthority
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockDebitOwners(ctx, tx, []DebitOwner{{ID: owner}}); err != nil {
		return err
	}
	if err = s.syncOwnerTx(ctx, tx, owner); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *DebitStore) syncOwnerTx(ctx context.Context, tx pgx.Tx, owner string) error {
	w, err := CurrentBudgetWindows(ctx, tx, time.Now())
	if err != nil {
		return err
	}
	var version int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM olp.execution_debit_versions WHERE owner_id=$1),0)`, owner).Scan(&version); err != nil {
		return err
	}
	for _, basis := range []Basis{BasisCost, BasisCharge} {
		var amount string
		var blocked bool
		if err = tx.QueryRow(ctx, `SELECT COALESCE(sum(GREATEST(0,o.bound-CASE WHEN o.basis='charge' THEN d.observed_charge ELSE d.observed_cost END)),0)::text,COALESCE(bool_or(d.bound_violation OR d.state='conflict'),false)
 FROM olp.execution_debit_owners o JOIN olp.execution_debits d ON d.id=o.dispatch_id
 WHERE o.owner_id=$1 AND o.basis=$2 AND d.closed_at IS NULL`, owner, basis).Scan(&amount, &blocked); err != nil {
			return err
		}
		snapshot := CostSnapshot{CostOwnerID: owner, Basis: basis, DailyWindowID: w.DailyID, WeeklyWindowID: w.WeeklyID, MonthlyWindowID: w.MonthlyID, DailyStart: w.DailyStart, DailyEnd: w.DailyEnd, WeeklyStart: w.WeeklyStart, WeeklyEnd: w.WeeklyEnd, MonthlyStart: w.MonthlyStart, MonthlyEnd: w.MonthlyEnd, durable: &debitProjection{Version: version, Amount: amount, Blocked: blocked}}
		rows, e := tx.Query(ctx, `SELECT window_kind,sum(CASE WHEN $2='charge' THEN charged ELSE accrued END)::text,
 sum(CASE WHEN $2='charge' THEN uncharged_attempts ELSE unpriced_attempts END)::bigint,bool_and(weekly_complete)
 FROM olp.execution_owner_spend WHERE owner_id=$1 AND ((window_kind='day' AND window_id=$3) OR (window_kind='month' AND window_id=$4) OR (window_kind='week' AND window_id=$5)) GROUP BY window_kind`, owner, string(basis), w.DailyID, w.MonthlyID, w.WeeklyID)
		if e != nil {
			return e
		}
		for rows.Next() {
			var kind, total string
			var missing int64
			var complete bool
			if e = rows.Scan(&kind, &total, &missing, &complete); e != nil {
				rows.Close()
				return e
			}
			switch kind {
			case "day":
				snapshot.DailyAccrued = total
			case "month":
				snapshot.MonthlyAccrued = total
				snapshot.UnpricedAttempts = missing
			case "week":
				if complete {
					snapshot.WeeklyAccrued = total
				}
			}
		}
		rows.Close()
		if e = rows.Err(); e != nil {
			return e
		}
		// Absence of accounting evidence must remain absence, including a newly
		// enabled weekly cap. Ordinary reconciliation establishes completeness.
		if snapshot.DailyAccrued == "" || snapshot.MonthlyAccrued == "" {
			return ErrUninitializedCost
		}
		if _, _, err = s.Limiter.ApplyCostSnapshot(ctx, snapshot); err != nil {
			return err
		}
	}
	s.Limiter.recordDebitVersion(owner, version)
	return nil
}

// CheckOwnerAccounting replaces the existing pending-accounting read at cost
// admission. The same database snapshot supplies the ledger generation, so a
// regional projection cannot remain stale until its background reconciliation.
func (s *DebitStore) CheckOwnerAccounting(ctx context.Context, owner string) error {
	var pending bool
	var version int64
	if err := s.Pool.QueryRow(ctx, `SELECT olp.has_pending_resource_usage($1::uuid),COALESCE((SELECT version FROM olp.execution_debit_versions WHERE owner_id=$1::uuid),0)`, owner).Scan(&pending, &version); err != nil {
		return err
	}
	if pending {
		return ErrUninitializedCost
	}
	if s.Limiter.debitVersionCurrent(owner, version) {
		return nil
	}
	return s.SyncOwner(ctx, owner)
}

func (l *Limiter) debitVersionCurrent(owner string, version int64) bool {
	if version == 0 {
		return true
	}
	l.debitVersionMu.Lock()
	defer l.debitVersionMu.Unlock()
	return l.debitVersions[owner] == version
}

func (l *Limiter) recordDebitVersion(owner string, version int64) {
	if version == 0 {
		return
	}
	l.debitVersionMu.Lock()
	defer l.debitVersionMu.Unlock()
	// This cache contains no exposure or authority. Eviction only causes a
	// fresh, fenced synchronization, never the release of a durable hold.
	if l.debitVersions == nil {
		l.debitVersions = make(map[string]int64)
	}
	if len(l.debitVersions) >= 8192 {
		clear(l.debitVersions)
	}
	l.debitVersions[owner] = version
}

func (l *Limiter) verifyDurableCost(ctx context.Context, r Request) error {
	if !r.CostBasis.valid() || !canonicalID(r.CostOwnerID) {
		return ErrDebitConflict
	}
	for _, cap := range []*string{r.DailyCostLimit, r.WeeklyCostLimit, r.MonthlyCostLimit} {
		if cap != nil && !ValidCostLimit(*cap) {
			return ErrDebitConflict
		}
	}
	k := l.keysFor(r)
	optional := func(v *string) string {
		if v == nil {
			return ""
		}
		return *v
	}
	value, err := l.eval(ctx, reserveCostScript, []string{k.dailyCost, k.monthlyCost, k.pending, k.expiry, k.weeklyCost, l.costPrefixFor(r.CostOwnerID, r.CostBasis) + ":durable"}, optional(r.DailyCostLimit), optional(r.MonthlyCostLimit), "0", "verify", "", "0", optional(r.WeeklyCostLimit), r.CostIncreases)
	if err != nil {
		return err
	}
	result, err := parseCostReservation(value)
	if err != nil {
		return err
	}
	switch result.kind {
	case resultGranted:
		return nil
	case resultUninitialized:
		return ErrUninitializedCost
	case resultMalformed:
		return ErrMalformedState
	case resultRejected:
		return &ExceededError{Dimension: result.dimension, RetryAfter: time.Duration(result.retryAfterMS) * time.Millisecond, Basis: r.CostBasis}
	}
	return ErrUnexpectedResponse
}

// ReconcileDebits refreshes existing projections and fences abandoned prepared
// work. It has no remote IO and cannot resume a model/tool/agent step.
func (s *DebitStore) ReconcileDebits(ctx context.Context) error {
	if _, err := s.RecoverPrepared(ctx, 256); err != nil {
		return err
	}
	rows, err := s.Pool.Query(ctx, `SELECT owner_id::text FROM olp.execution_debit_versions ORDER BY owner_id`)
	if err != nil {
		return err
	}
	var owners []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		owners = append(owners, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, owner := range owners {
		if err = s.SyncOwner(ctx, owner); err != nil && !errors.Is(err, ErrUninitializedCost) {
			return err
		}
	}
	return nil
}

func appendDebitProjection(args []string, p *debitProjection) ([]string, error) {
	if p.Version < 0 || p.Version > maxLuaInteger || !validDecimal(p.Amount) || len(args) != 15 {
		return nil, ErrDebitConflict
	}
	return append(args, strconv.FormatInt(p.Version, 10), p.Amount, switchArg(p.Blocked)), nil
}

// A warm projection is enforced in the existing Valkey round trip. Only an
// absent projection is rebuilt synchronously; transient lease expiry never
// clears durable exposure and transport ambiguity is never retried here.
func (l *Limiter) evalDurableCost(ctx context.Context, owner string, keys []string, args ...string) (any, error) {
	value, err := l.eval(ctx, reserveCostScript, keys, args...)
	if err != nil {
		return nil, err
	}
	result, parseErr := parseCostReservation(value)
	if parseErr != nil || !result.durableMissing {
		return value, nil
	}
	if err = l.syncDurable(ctx, owner); err != nil {
		return nil, err
	}
	return l.eval(ctx, reserveCostScript, keys, args...)
}
