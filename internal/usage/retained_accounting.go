package usage

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/limits"
)

// PersistRetainedEventTx keeps the accepted accounting pins and also charges a
// budget group attached to the key after acceptance. The key lock serializes
// that changing boundary with this atomic financial commit.
func PersistRetainedEventTx(ctx context.Context, tx pgx.Tx, event *Event, payload []byte) (Persisted, error) {
	var currentGroup *string
	if err := tx.QueryRow(ctx, `SELECT budget_group_id::text FROM olp.api_keys WHERE id=$1 FOR SHARE`, event.APIKeyID).Scan(&currentGroup); err != nil {
		return Persisted{}, err
	}
	result, err := PersistEventTx(ctx, tx, event, payload)
	if err != nil || result.Outcome != PersistOutcomePersisted {
		return result, err
	}
	if currentGroup == nil || event.BudgetGroupID != nil && *currentGroup == *event.BudgetGroupID {
		return result, invalidateRetainedCostProjections(ctx, tx, result.CostSnapshots)
	}
	var facts, unpriced, uncharged int64
	var cost, charge string
	if err = tx.QueryRow(ctx, factTotalsSQL, event.EventID, nil).Scan(&facts, &cost, &unpriced, &charge, &uncharged); err != nil {
		return Persisted{}, err
	}
	if facts == 0 {
		return result, nil
	}
	snapshots, err := limits.AddGroupCostDelta(ctx, tx, *currentGroup, event.ObservedAt, cost, unpriced, charge, uncharged)
	if err != nil {
		return Persisted{}, err
	}
	for i := range snapshots {
		snapshots[i].RequestID = event.RequestID
	}
	result.CostSnapshots = append(result.CostSnapshots, snapshots...)
	return result, invalidateRetainedCostProjections(ctx, tx, result.CostSnapshots)
}

// Admission reads this generation in the same snapshot as pending liabilities.
// On financial commit, every replica must refresh its limiter projection before
// admitting a newly capped caller, even if this process dies before publication.
func invalidateRetainedCostProjections(ctx context.Context, tx pgx.Tx, snapshots []limits.CostSnapshot) error {
	owners := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		owners = append(owners, snapshot.CostOwnerID)
	}
	slices.Sort(owners)
	for _, owner := range slices.Compact(owners) {
		if _, err := tx.Exec(ctx, `INSERT INTO olp.execution_debit_versions(owner_id,version) VALUES($1,1) ON CONFLICT(owner_id) DO UPDATE SET version=olp.execution_debit_versions.version+1`, owner); err != nil {
			return err
		}
	}
	return nil
}
