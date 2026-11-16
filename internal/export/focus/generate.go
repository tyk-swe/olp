package focus

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/export/destination"
)

const (
	dirtyVersionSQL = `SELECT COALESCE((SELECT version FROM olp.focus_dirty_days WHERE day=$1::date),0)`

	nextRevisionSQL = `SELECT COALESCE((SELECT revision FROM olp.focus_datasets WHERE sink_id=$1::uuid AND charge_date=$2::date),0)+1`

	installationSQL = `SELECT id::text,name,COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton),'USD')
 FROM olp.installation WHERE singleton`
)

type snapshot struct {
	dataset      dataset
	dirtyVersion int64
	revision     int64
	usage        []usageRow
	purchases    []purchase
	facts        facts
}

type generated struct {
	dirtyVersion int64
	generatedAt  time.Time
	commitments  *CommitmentTotals
	assembled
}

func (w *Worker) generate(ctx context.Context, s sink, day time.Time) (generated, error) {
	generatedAt := w.Now().UTC()
	snap, err := w.snapshot(ctx, s, day, generatedAt)
	if err != nil {
		return generated{}, err
	}
	a, err := assemble(snap.dataset, snap.revision, generatedAt, w.Version, snap.usage, snap.purchases, snap.facts)
	if err != nil {
		return generated{}, err
	}
	return generated{dirtyVersion: snap.dirtyVersion, generatedAt: generatedAt, commitments: snap.facts.commitments, assembled: a}, nil
}

func (w *Worker) snapshot(ctx context.Context, s sink, day, now time.Time) (snapshot, error) {
	tx, err := w.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return snapshot{}, err
	}
	defer tx.Rollback(ctx)
	var dirtyVersion, revision int64
	if err := tx.QueryRow(ctx, dirtyVersionSQL, day).Scan(&dirtyVersion); err != nil {
		return snapshot{}, err
	}
	if err := tx.QueryRow(ctx, nextRevisionSQL, s.id, day).Scan(&revision); err != nil {
		return snapshot{}, err
	}
	d := dataset{sinkID: s.id, sinkName: s.name, day: day, basis: s.basis}
	if err := tx.QueryRow(ctx, installationSQL).Scan(&d.installationID, &d.installationName, &d.currency); err != nil {
		return snapshot{}, err
	}
	usage, err := collect(ctx, tx, scanUsageRow, s.basis.rowsSQL, day, s.scope.project, s.scope.route)
	if err != nil {
		return snapshot{}, err
	}
	purchases, err := readPurchases(ctx, tx, s, day)
	if err != nil {
		return snapshot{}, err
	}
	f, err := readFacts(ctx, tx, s, day, now)
	if err != nil {
		return snapshot{}, err
	}
	return snapshot{dataset: d, dirtyVersion: dirtyVersion, revision: revision, usage: usage, purchases: purchases, facts: f}, nil
}

func readPurchases(ctx context.Context, tx pgx.Tx, s sink, day time.Time) ([]purchase, error) {
	if s.basis.billsCallers() || s.scope.scoped() {
		return nil, nil
	}
	return collect(ctx, tx, scanPurchase, purchasesSQL, day)
}

func readFacts(ctx context.Context, tx pgx.Tx, s sink, day, now time.Time) (facts, error) {
	var c completeness
	if err := tx.QueryRow(ctx, s.basis.countsSQL, day, s.scope.project, s.scope.route).Scan(&c.IncludedAttempts,
		&c.FallbackLineAttempts, &c.UnpricedAttempts, &c.IncompleteAttempts, &c.UnchargedAttempts,
		&c.CallerPaidExcludedAttempts, &c.IngestionGap); err != nil {
		return facts{}, err
	}
	if err := tx.QueryRow(ctx, s.basis.unavailableSQL, day, s.scope.project, s.scope.route).Scan(&c.RawDetailUnavailable); err != nil {
		return facts{}, err
	}
	var accounting *string
	if err := tx.QueryRow(ctx, s.basis.accountedSQL, day, s.scope.project, s.scope.route).Scan(&accounting); err != nil {
		return facts{}, err
	}
	if s.basis.billsCallers() {
		return facts{completeness: c, accounting: accounting}, nil
	}
	commitments, err := readCommitmentTotals(ctx, tx, s, day, now)
	return facts{completeness: c, accounting: accounting, commitments: commitments}, err
}

func collect[T any](ctx context.Context, tx pgx.Tx, scan pgx.RowToFunc[T], sql string, args ...any) ([]T, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scan)
}

func scanUsageRow(row pgx.CollectableRow) (usageRow, error) {
	var (
		r                          usageRow
		consumer, model, operation *string
	)
	if err := row.Scan(&consumer, &r.consumerName, &r.providerID, &r.connectionName, &r.vendorID, &r.providerKind,
		&model, &operation, &r.route, &r.apiKeyID, &r.attribution, &r.committed, &r.commitmentPeriod, nil,
		&r.component, &r.tier, &r.unit, &r.listUnitPrice, &r.contractedUnitPrice, &r.quantity, &r.listCost,
		&r.contractedCost, &r.periodWeight, &r.latestAmount, &r.attempts, &r.scheduleID, &r.allocated); err != nil {
		return usageRow{}, err
	}
	r.consumer, r.model, r.operation = deref(consumer), deref(model), deref(operation)
	return r, nil
}

func scanPurchase(row pgx.CollectableRow) (purchase, error) {
	var p purchase
	err := row.Scan(&p.providerID, &p.period, &p.bucketStart, &p.amount, &p.connectionName,
		&p.vendorID, &p.providerKind, &p.scheduleID, &p.unused)
	return p, err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

type assembled struct {
	objects   []destination.Object
	rowCount  int64
	partCount int
	sum       totals
}

func assemble(d dataset, revision int64, generatedAt time.Time, generator string, usage []usageRow, purchases []purchase, f facts) (assembled, error) {
	rows := make([][]string, 0, len(usage)+2*len(purchases))
	for _, r := range usage {
		rows = append(rows, render(r, d))
	}
	for _, p := range purchases {
		rows = append(rows, purchaseRows(p, d)...)
	}
	sum, err := sumRows(rows)
	if err != nil {
		return assembled{}, err
	}
	parts, err := buildParts(d.day, revision, rows)
	if err != nil {
		return assembled{}, err
	}
	manifest, err := buildManifest(d, revision, generatedAt, generator, parts, sum, f)
	if err != nil {
		return assembled{}, err
	}
	objects := make([]destination.Object, 0, len(parts)+1)
	for _, p := range parts {
		objects = append(objects, destination.Object{Key: p.Key, ContentType: "application/gzip", Body: p.body})
	}
	return assembled{
		objects:   append(objects, destination.Object{Key: manifestKey(d.day), ContentType: "application/json", Body: manifest}),
		rowCount:  int64(len(rows)),
		partCount: len(parts),
		sum:       sum,
	}, nil
}
