package projects

import (
	"context"
	"maps"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

func ReadProjectNames(ctx context.Context, q access.Queryer) (map[string]string, error) {
	return readNames(ctx, q, "SELECT id::text,name FROM olp.projects")
}

func ReadOrganizationNames(ctx context.Context, q access.Queryer) (map[string]string, error) {
	return readNames(ctx, q, "SELECT id::text,name FROM olp.organizations")
}

func OrganizationName(ctx context.Context, q access.Queryer, id string) (string, error) {
	var name string
	err := q.QueryRow(ctx, "SELECT name FROM olp.organizations WHERE id=$1", id).Scan(&name)
	return name, err
}

func readNames(ctx context.Context, q access.Queryer, query string) (map[string]string, error) {
	rows, err := q.Query(ctx, query)
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
