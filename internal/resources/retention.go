package resources

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

const expiryBatch = 100

type ExpiryFailures struct {
	Count int
	first error
}

func (e *ExpiryFailures) Error() string {
	return fmt.Sprintf("%d resource expiry owners refused accounting", e.Count)
}

func (e *ExpiryFailures) Unwrap() error { return e.first }

func (e *ExpiryFailures) record(err error) {
	if e.first == nil {
		e.first = err
	}
	e.Count++
}

type expiryCursor struct {
	at time.Time
	id uuid.UUID
}

type expiryResult struct {
	visited int
	removed int64
	next    *expiryCursor
	failed  ExpiryFailures
}

type expiredResource struct {
	id      uuid.UUID
	at      time.Time
	pending []byte
}

func (s *Store) Expire(ctx context.Context, now time.Time) (int64, error) {
	var removed int64
	var cursor *expiryCursor
	var failed ExpiryFailures
	for ctx.Err() == nil {
		result, err := expireBatch(ctx, s.pool, now, cursor)
		if err != nil {
			return removed, err
		}
		removed += result.removed
		if failed.first == nil {
			failed.first = result.failed.first
		}
		failed.Count += result.failed.Count
		cursor = result.next
		if result.visited < expiryBatch {
			if failed.Count > 0 {
				return removed, &failed
			}
			return removed, nil
		}
	}
	return removed, ctx.Err()
}

func expireBatch(ctx context.Context, pool *pgxpool.Pool, now time.Time, cursor *expiryCursor) (expiryResult, error) {
	return withTxValue(ctx, pool, func(tx pgx.Tx) (expiryResult, error) {
		batch, err := loadExpiredBatch(ctx, tx, now, cursor)
		if err != nil {
			return expiryResult{}, err
		}
		result := expiryResult{visited: len(batch)}
		for _, resource := range batch {
			result.next = &expiryCursor{at: resource.at, id: resource.id}
			accountErr, txErr := reclaimExpiredResource(ctx, tx, resource, now)
			if txErr != nil {
				return expiryResult{}, txErr
			}
			if accountErr != nil {
				result.failed.record(accountErr)
				continue
			}
			result.removed++
		}
		return result, nil
	})
}

func loadExpiredBatch(ctx context.Context, tx pgx.Tx, now time.Time, cursor *expiryCursor) ([]expiredResource, error) {
	var after any
	id := uuid.Nil
	if cursor != nil {
		after = cursor.at
		id = cursor.id
	}
	rows, err := tx.Query(ctx, `SELECT id,expires_at,
 CASE WHEN olp.resource_owns_pending_usage(kind) THEN metadata->'pending_usage' END FROM olp.provider_resources
 WHERE expires_at IS NOT NULL AND expires_at<$1
 AND NOT EXISTS(SELECT 1 FROM olp.retained_submissions rs WHERE rs.resource_id=olp.provider_resources.id)
 AND NOT EXISTS(SELECT 1 FROM olp.retained_settlements rs WHERE rs.resource_id=olp.provider_resources.id)
 AND ($3::timestamptz IS NULL OR (expires_at,id)>($3,$4)) ORDER BY expires_at,id
 LIMIT $2 FOR UPDATE SKIP LOCKED`, now, expiryBatch, after, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	batch := make([]expiredResource, 0, expiryBatch)
	for rows.Next() {
		var resource expiredResource
		if err := rows.Scan(&resource.id, &resource.at, &resource.pending); err != nil {
			return nil, err
		}
		batch = append(batch, resource)
	}
	return batch, rows.Err()
}

func reclaimExpiredResource(ctx context.Context, tx pgx.Tx, resource expiredResource, now time.Time) (error, error) {
	child, err := tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	_, err = persistPendingUsage(ctx, child, resource.pending, usage.AttemptUsage{Evidence: usage.EvidenceUnobservedUncertain}, now, false)
	if err == nil {
		_, err = child.Exec(ctx, `DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2`, resource.id, secrets.ProviderContinuation)
	}
	if err == nil {
		_, err = child.Exec(ctx, `DELETE FROM olp.provider_resources WHERE id=$1`, resource.id)
	}
	if err != nil {
		if rollbackErr := child.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil {
			return nil, rollbackErr
		}
		return err, nil
	}
	if err := child.Commit(ctx); err != nil {
		return nil, err
	}
	return nil, nil
}
