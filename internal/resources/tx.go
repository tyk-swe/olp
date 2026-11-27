package resources

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/retainedapi"
)

func withTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func withTxValue[T any](ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) (T, error)) (T, error) {
	var zero T
	tx, err := pool.Begin(ctx)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	value, err := fn(tx)
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, err
	}
	return value, nil
}

func withReadTxValue[T any](ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) (T, error)) (T, error) {
	var zero T
	tx, err := pool.Begin(ctx)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	return fn(tx)
}

func (s *Store) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return withTx(ctx, s.pool, fn)
}

func newUUIDv7() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("provider resource id: %w", err)
	}
	return id, nil
}

func ParseLocalID(local string) (string, uuid.UUID, error) {
	i := strings.LastIndexByte(local, '_')
	if i <= 0 {
		return "", uuid.Nil, ErrNotFound
	}
	kind, rest := local[:i], local[i+1:]
	if len(rest) != 32 {
		return "", uuid.Nil, ErrNotFound
	}
	id, err := uuid.Parse(rest[:8] + "-" + rest[8:12] + "-" + rest[12:16] + "-" + rest[16:20] + "-" + rest[20:])
	if err != nil || id == uuid.Nil || LocalID(kind, id) != local {
		return "", uuid.Nil, ErrNotFound
	}
	if !knownResourceKind(kind) {
		return "", uuid.Nil, ErrNotFound
	}
	return kind, id, nil
}

func knownResourceKind(kind string) bool {
	switch kind {
	case KindFile, KindBatch, KindResponse, KindContinuation, KindStrictResponse, KindInteraction,
		KindStrictFile, KindStrictBatch, KindAnthropicFile, KindAnthropicBatch, KindGeminiUpload,
		KindGeminiFile, KindGeminiBatch, KindCloudBatch:
		return true
	default:
		return slices.Contains(retainedapi.Kinds(), kind)
	}
}
