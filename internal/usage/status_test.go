package usage

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// consumerRow replays one stored health row into the scan targets
// ReadConsumerStatus uses, so the classification can be tested without a
// database.
type consumerRow struct {
	values []any
	err    error
}

func (r consumerRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("unexpected scan arity")
	}
	for i, target := range dest {
		switch typed := target.(type) {
		case *int64:
			typed2, ok := r.values[i].(int64)
			if !ok {
				return errors.New("unexpected scan type")
			}
			*typed = typed2
		case **time.Time:
			at, ok := r.values[i].(*time.Time)
			if !ok && r.values[i] != nil {
				return errors.New("unexpected scan type")
			}
			*typed = at
		case *time.Time:
			at, ok := r.values[i].(time.Time)
			if !ok {
				return errors.New("unexpected scan type")
			}
			*typed = at
		default:
			return errors.New("unexpected scan target")
		}
	}
	return nil
}

type consumerQueryer struct{ row pgx.Row }

func (c consumerQueryer) QueryRow(context.Context, string, ...any) pgx.Row { return c.row }
func (c consumerQueryer) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unused")
}
