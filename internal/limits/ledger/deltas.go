package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/limits"
)

type Delta struct {
	ObservedAt time.Time
	Cost       string
	Unpriced   int64
	Charge     string
	Uncharged  int64
}

func (d Delta) validate() error {
	switch {
	case !limits.ValidDecimal(d.Cost):
		return fmt.Errorf("cost delta %q is not a non-negative decimal with at most 12 fractional digits", d.Cost)
	case !limits.ValidDecimal(d.Charge):
		return fmt.Errorf("charge delta %q is not a non-negative decimal with at most 12 fractional digits", d.Charge)
	case d.Unpriced < 0 || d.Unpriced > limits.MaxCounter:
		return fmt.Errorf("unpriced attempt count %d is out of range", d.Unpriced)
	case d.Uncharged < 0 || d.Uncharged > limits.MaxCounter:
		return fmt.Errorf("uncharged attempt count %d is out of range", d.Uncharged)
	}
	return nil
}

var deltaSQL = map[limits.OwnerKind]string{
	limits.OwnerKey:       costDeltaSQL(keyWindows),
	limits.OwnerGroup:     costDeltaSQL(groupWindows),
	limits.OwnerEndUser:   costDeltaSQL(endUserWindows),
	limits.OwnerAggregate: costDeltaSQL(aggregateWindows),
}

func AddDelta(ctx context.Context, tx pgx.Tx, kind limits.OwnerKind, ownerID string, d Delta) ([]limits.Snapshot, error) {
	if _, err := uuid.Parse(ownerID); err != nil {
		return nil, fmt.Errorf("%s budget owner ID %q is not a UUID", kind, ownerID)
	}
	if err := d.validate(); err != nil {
		return nil, err
	}
	if kind == limits.OwnerSupply {
		return scanSupply(tx.QueryRow(ctx, addSupplyCostDeltaSQL, ownerID, d.Cost, d.Unpriced, d.ObservedAt))
	}
	query, ok := deltaSQL[kind]
	if !ok {
		return nil, fmt.Errorf("budget owner kind %q keeps no cost windows", kind)
	}
	return scanPair(tx.QueryRow(ctx, query, ownerID, d.Cost, d.Unpriced, d.Charge, d.Uncharged, d.ObservedAt))
}

func AddEndUserDelta(ctx context.Context, tx pgx.Tx, keyID, digest string, d Delta) ([]limits.Snapshot, error) {
	var projectID *string
	if err := tx.QueryRow(ctx, "SELECT project_id::text FROM olp.api_keys WHERE id=$1", keyID).Scan(&projectID); err != nil {
		return nil, err
	}
	snapshots, err := addEndUserAccountDelta(ctx, tx, limits.EndUserKeyID(keyID, digest), &keyID, nil, digest, d)
	if err != nil || projectID == nil {
		return snapshots, err
	}
	project, err := addEndUserAccountDelta(ctx, tx, limits.EndUserProjectID(*projectID, digest), nil, projectID, digest, d)
	if err != nil {
		return nil, err
	}
	return append(snapshots, project...), nil
}

func addEndUserAccountDelta(ctx context.Context, tx pgx.Tx, accountID string, keyID, projectID *string, digest string, d Delta) ([]limits.Snapshot, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO olp.end_user_accounts (id,api_key_id,project_id,end_user_digest)
			VALUES ($1,$2,$3,$4) ON CONFLICT (id) DO NOTHING`, accountID, keyID, projectID, digest)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() > 0 {
		return scanPair(tx.QueryRow(ctx, endUserSnapshotsSQL, d.ObservedAt, accountID))
	}
	return AddDelta(ctx, tx, limits.OwnerEndUser, accountID, d)
}

type AggregateEvent struct {
	EventID    string
	AttemptID  *string
	KeyID      string
	ProviderID *string
	Route      string
	Labels     map[string]string
	At         time.Time
}

type aggregateDelta struct {
	id    string
	delta Delta
}

func AddAggregateDeltas(ctx context.Context, tx pgx.Tx, e AggregateEvent) ([]limits.Snapshot, error) {
	rows, err := tx.Query(ctx, aggregateEventDeltasSQL, e.KeyID, e.ProviderID, e.Route, attribution.JSON(e.Labels), e.EventID, e.AttemptID)
	if err != nil {
		return nil, err
	}
	deltas, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (aggregateDelta, error) {
		owed := aggregateDelta{delta: Delta{ObservedAt: e.At}}
		err := row.Scan(&owed.id, &owed.delta.Cost, &owed.delta.Unpriced, &owed.delta.Charge, &owed.delta.Uncharged)
		return owed, err
	})
	if err != nil {
		return nil, err
	}
	snapshots := make([]limits.Snapshot, 0, 2*len(deltas))
	for _, owed := range deltas {
		applied, err := addAggregateDelta(ctx, tx, owed)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, applied...)
	}
	return snapshots, nil
}

func addAggregateDelta(ctx context.Context, tx pgx.Tx, owed aggregateDelta) ([]limits.Snapshot, error) {
	tag, err := tx.Exec(ctx, `UPDATE olp.aggregate_budget_accounts SET initialized=true WHERE id=$1 AND NOT initialized`, owed.id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() > 0 {
		return scanPair(tx.QueryRow(ctx, aggregateSnapshotsSQL, owed.delta.ObservedAt, owed.id))
	}
	return AddDelta(ctx, tx, limits.OwnerAggregate, owed.id, owed.delta)
}
