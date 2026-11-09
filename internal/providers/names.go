package providers

import (
	"context"
	"maps"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

func ReadProviderNames(ctx context.Context, q access.Queryer) (map[string]string, error) {
	rows, err := q.Query(ctx, "SELECT id::text,name FROM olp.providers")
	if err != nil {
		return nil, err
	}
	type named struct{ ID, Name string }
	entries, err := pgx.CollectRows(rows, pgx.RowToStructByPos[named])
	if err != nil {
		return nil, err
	}
	return maps.Collect(func(yield func(string, string) bool) {
		for _, entry := range entries {
			if !yield(entry.ID, entry.Name) {
				return
			}
		}
	}), nil
}
