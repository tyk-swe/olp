package ledger

import (
	"context"

	"github.com/jackc/pgx/v5"
)

func LoadBudgetIncreases(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT owner_id::text,jsonb_agg(jsonb_build_object('window',window_kind,'window_id',window_id,'amount',amount::text,'starts_at',floor(extract(epoch FROM starts_at)*1000)::bigint,'expires_at',floor(extract(epoch FROM expires_at)*1000)::bigint) ORDER BY id)::text FROM olp.budget_increases WHERE revoked_at IS NULL AND expires_at>now() GROUP BY owner_id`)
	if err != nil {
		return nil, err
	}
	grants, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var owner, increases string
		err := row.Scan(&owner, &increases)
		return [2]string{owner, increases}, err
	})
	if err != nil || len(grants) == 0 {
		return nil, err
	}
	result := make(map[string]string, len(grants))
	for _, grant := range grants {
		result[grant[0]] = grant[1]
	}
	return result, nil
}
