package limits

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// LoadBudgetIncreases compiles bounded server-authored grants once per authority
// refresh. Valkey, not the gateway clock, checks their period and expiration.
func LoadBudgetIncreases(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT owner_id::text,jsonb_agg(jsonb_build_object('window',window_kind,'window_id',window_id,'amount',amount::text,'starts_at',floor(extract(epoch FROM starts_at)*1000)::bigint,'expires_at',floor(extract(epoch FROM expires_at)*1000)::bigint) ORDER BY id)::text FROM olp.budget_increases WHERE revoked_at IS NULL AND expires_at>now() GROUP BY owner_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result map[string]string
	for rows.Next() {
		var owner, grants string
		if err = rows.Scan(&owner, &grants); err != nil {
			return nil, err
		}
		if result == nil {
			result = map[string]string{}
		}
		result[owner] = grants
	}
	return result, rows.Err()
}
