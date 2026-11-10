package projects

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/ledger"
)

// Install the returned balances before commit, or admission ignores the project's past spend.
func AssignOrganization(ctx context.Context, tx pgx.Tx, project, organization string) ([]limits.Snapshot, error) {
	if _, err := tx.Exec(ctx, "UPDATE olp.projects SET organization_id=$2,etag=$3,updated_at=now() WHERE id=$1", project, organization, access.NewID()); err != nil {
		return nil, err
	}
	snapshots, err := ledger.ReconcileAggregateBudget(ctx, tx, "organization", organization, time.Now())
	if err != nil {
		return nil, err
	}
	if _, err := access.AdvanceAuthority(ctx, tx); err != nil {
		return nil, err
	}
	return snapshots, nil
}
