package access

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

const expireKeyOverlapsSQL = `DELETE FROM olp.api_key_overlaps WHERE expires_at<=now()
 OR api_key_id IN (SELECT id FROM olp.api_keys WHERE revoked_at IS NOT NULL)`

func ExpireKeyOverlaps(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, expireKeyOverlapsSQL)
	return err
}
