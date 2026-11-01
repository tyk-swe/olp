package grants

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func WithRefreshLock[T any](ctx context.Context, conn *pgx.Conn, key string, wait bool, run func() (T, error)) (T, error) {
	var zero T
	if wait {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1,$2))", key, refreshLockSeed); err != nil {
			return zero, err
		}
	} else {
		var held bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1,$2))", key, refreshLockSeed).Scan(&held); err != nil || !held {
			return zero, err
		}
	}
	result, runErr := run()
	_, unlockErr := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(hashtextextended($1,$2))", key, refreshLockSeed)
	return result, errors.Join(runErr, unlockErr)
}
