package usage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

type consumerQueryer struct{ row pgx.Row }

func (c consumerQueryer) QueryRow(context.Context, string, ...any) pgx.Row { return c.row }
func (c consumerQueryer) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unused")
}
