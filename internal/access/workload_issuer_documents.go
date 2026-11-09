package access

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/workload"
)

type StoredWorkloadIssuer struct {
	ID     string
	Config workload.Config
	ETag   string
}

const selectWorkloadIssuer = "SELECT id::text,document,etag::text FROM olp.workload_issuers WHERE issuer=$1"

func ReadWorkloadIssuerDocuments(ctx context.Context, q Queryer) ([]workload.Config, error) {
	rows, err := q.Query(ctx, "SELECT document FROM olp.workload_issuers ORDER BY issuer")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[workload.Config])
}

func FindWorkloadIssuer(ctx context.Context, q Queryer, issuer string) (StoredWorkloadIssuer, bool, error) {
	return scanWorkloadIssuer(q.QueryRow(ctx, selectWorkloadIssuer, issuer))
}

func LockWorkloadIssuer(ctx context.Context, tx pgx.Tx, issuer string) (StoredWorkloadIssuer, bool, error) {
	return scanWorkloadIssuer(tx.QueryRow(ctx, selectWorkloadIssuer+" FOR UPDATE", issuer))
}

func scanWorkloadIssuer(row pgx.Row) (StoredWorkloadIssuer, bool, error) {
	var stored StoredWorkloadIssuer
	err := row.Scan(&stored.ID, &stored.Config, &stored.ETag)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredWorkloadIssuer{}, false, nil
	}
	return stored, err == nil, err
}
