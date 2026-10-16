package usage

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"
)

type recordingHealthExecer struct {
	calls [][]any
	sql   string
	err   error
}

func (r *recordingHealthExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.sql = sql
	r.calls = append(r.calls, args)
	return pgconn.CommandTag{}, r.err
}

// A nil pool proves the idle pass never reaches the database.
