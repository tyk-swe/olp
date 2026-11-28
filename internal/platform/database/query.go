package database

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/domain/problem"
)

type Pagination struct {
	Limit  int
	Before string
}

func NotFoundOr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return problem.NotFound
	}
	return err
}

func QueryJSON(ctx context.Context, db Queryer, sql string, args ...any) ([]map[string]any, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, jsonRow)
}

func jsonRow(row pgx.CollectableRow) (map[string]any, error) {
	var document []byte
	if err := row.Scan(&document); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var item map[string]any
	err := decoder.Decode(&item)
	return item, err
}
