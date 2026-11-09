package providers

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type ProviderName struct {
	ID   string
	Name string
}

func ReadProviderNames(ctx context.Context, q access.Queryer) ([]ProviderName, error) {
	rows, err := q.Query(ctx, "SELECT id::text,name FROM olp.providers")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ProviderName])
}
