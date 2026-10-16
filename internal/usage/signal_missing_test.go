package usage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

type missingSignalTx struct {
	pgx.Tx
}

func (missingSignalTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("missing measurements must not be treated as known recoveries")
}
