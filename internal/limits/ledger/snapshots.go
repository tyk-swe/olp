package ledger

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/limits"
)

type windowTotals struct {
	owner                             string
	windows                           limits.Windows
	dailyCost, weeklyCost, monthCost  string
	dailyCharge, weeklyCharge, charge string
	unpriced, uncharged               int64
}

func (t windowTotals) snapshots() ([]limits.Snapshot, error) {
	pair := []limits.Snapshot{
		{OwnerID: t.owner, Basis: limits.BasisCost, Windows: t.windows, DailyAccrued: t.dailyCost, WeeklyAccrued: t.weeklyCost, MonthlyAccrued: t.monthCost, UnpricedAttempts: t.unpriced},
		{OwnerID: t.owner, Basis: limits.BasisCharge, Windows: t.windows, DailyAccrued: t.dailyCharge, WeeklyAccrued: t.weeklyCharge, MonthlyAccrued: t.charge, UnpricedAttempts: t.uncharged},
	}
	for _, snapshot := range pair {
		if err := snapshot.Validate(); err != nil {
			return nil, err
		}
	}
	return pair, nil
}

func scanPair(row pgx.Row) ([]limits.Snapshot, error) {
	var t windowTotals
	w := &t.windows
	if err := row.Scan(&t.owner, &w.DailyID, &t.dailyCost, &t.dailyCharge, &w.MonthlyID, &t.monthCost, &t.charge,
		&t.unpriced, &t.uncharged, &w.WeeklyID, &t.weeklyCost, &t.weeklyCharge,
		&w.DailyStart, &w.DailyEnd, &w.MonthlyStart, &w.MonthlyEnd, &w.WeeklyStart, &w.WeeklyEnd); err != nil {
		return nil, err
	}
	return t.snapshots()
}

func scanSupply(row pgx.Row) ([]limits.Snapshot, error) {
	s := limits.Snapshot{Basis: limits.BasisCost}
	w := &s.Windows
	if err := row.Scan(&s.OwnerID, &w.DailyID, &s.DailyAccrued, &w.MonthlyID, &s.MonthlyAccrued, &s.UnpricedAttempts,
		&w.WeeklyID, &s.WeeklyAccrued, &w.DailyStart, &w.DailyEnd, &w.MonthlyStart, &w.MonthlyEnd, &w.WeeklyStart, &w.WeeklyEnd); err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return []limits.Snapshot{s}, nil
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func collect(ctx context.Context, db querier, scan func(pgx.Row) ([]limits.Snapshot, error), query string, args ...any) ([]limits.Snapshot, error) {
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	scanned, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([]limits.Snapshot, error) { return scan(row) })
	if err != nil {
		return nil, err
	}
	snapshots := make([]limits.Snapshot, 0, 2*len(scanned))
	for _, owner := range scanned {
		snapshots = append(snapshots, owner...)
	}
	return snapshots, nil
}

func Snapshots(ctx context.Context, db querier, now time.Time) ([]limits.Snapshot, error) {
	sources := [...]struct {
		scan  func(pgx.Row) ([]limits.Snapshot, error)
		query string
		args  []any
	}{
		{scanPair, reconciliationSnapshotsSQL, []any{now.UTC()}},
		{scanPair, reconciliationGroupSnapshotsSQL, []any{now.UTC()}},
		{scanSupply, supplySnapshotsSQL, []any{now}},
		{scanPair, endUserSnapshotsSQL, []any{now, nil}},
		{scanPair, aggregateSnapshotsSQL, []any{now, nil}},
	}
	all := []limits.Snapshot{}
	for _, source := range sources {
		snapshots, err := collect(ctx, db, source.scan, source.query, source.args...)
		if err != nil {
			return nil, err
		}
		all = append(all, snapshots...)
	}
	return all, nil
}

func OwnerSnapshots(ctx context.Context, db querier, kind limits.OwnerKind, ownerID string, at time.Time) ([]limits.Snapshot, error) {
	query, ok := map[limits.OwnerKind]string{limits.OwnerEndUser: endUserSnapshotsSQL, limits.OwnerAggregate: aggregateSnapshotsSQL}[kind]
	if !ok {
		return nil, limits.ErrUninitializedCost
	}
	snapshots, err := scanPair(db.QueryRow(ctx, query, at, ownerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, limits.ErrUninitializedCost
	}
	return snapshots, err
}

func ReconcileAggregateBudget(ctx context.Context, tx pgx.Tx, level, subject string, at time.Time) ([]limits.Snapshot, error) {
	snapshots, err := OwnerSnapshots(ctx, tx, limits.OwnerAggregate, limits.AggregateBudgetID(level, subject), at)
	if errors.Is(err, limits.ErrUninitializedCost) {
		return nil, nil
	}
	return snapshots, err
}
