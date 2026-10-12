package resources

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

const expiryBatch = 100

// Expire reclaims bounded, separately committed batches. Missing terminal
// usage becomes durable uncertainty before its only owner is removed. Row
// locks fence concurrent observers and SKIP LOCKED lets replicas share work.
type ExpiryFailures struct {
	Count int
	first error
}

func (e *ExpiryFailures) Error() string {
	return fmt.Sprintf("%d resource expiry owners refused accounting", e.Count)
}
func (e *ExpiryFailures) Unwrap() error { return e.first }

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

func (s *Store) Expire(ctx context.Context, now time.Time) (int64, error) {
	var removed int64
	var cursor *expiryCursor
	var failed ExpiryFailures
	for ctx.Err() == nil {
		result, err := s.expireBatch(ctx, now, cursor)
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

func (s *Store) expireBatch(ctx context.Context, now time.Time, cursor *expiryCursor) (expiryResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return expiryResult{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var after any
	id := uuid.Nil
	if cursor != nil {
		after = cursor.at
		id = cursor.id
	}
	rows, err := tx.Query(ctx, `SELECT id,expires_at,
 CASE WHEN olp.resource_owns_pending_usage(kind) THEN metadata->'pending_usage' END FROM olp.provider_resources
 WHERE expires_at IS NOT NULL AND expires_at<$1
 AND ($3::timestamptz IS NULL OR (expires_at,id)>($3,$4)) ORDER BY expires_at,id
 LIMIT $2 FOR UPDATE SKIP LOCKED`, now, expiryBatch, after, id)
	if err != nil {
		return expiryResult{}, err
	}
	type expired struct {
		id      uuid.UUID
		at      time.Time
		pending []byte
	}
	batch := make([]expired, 0, expiryBatch)
	for rows.Next() {
		var resource expired
		if err = rows.Scan(&resource.id, &resource.at, &resource.pending); err != nil {
			rows.Close()
			return expiryResult{}, err
		}
		batch = append(batch, resource)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return expiryResult{}, err
	}
	result := expiryResult{visited: len(batch)}
	for _, resource := range batch {
		result.next = &expiryCursor{at: resource.at, id: resource.id}
		child, err := tx.Begin(ctx)
		if err != nil {
			return expiryResult{}, err
		}
		_, err = persistPendingUsage(ctx, child, resource.pending, usage.AttemptUsage{BillingUncertain: true}, now, false)
		if err == nil {
			_, err = child.Exec(ctx, `DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2`, resource.id, secrets.ProviderContinuation)
		}
		if err == nil {
			_, err = child.Exec(ctx, `DELETE FROM olp.provider_resources WHERE id=$1`, resource.id)
		}
		if err != nil {
			if rollbackErr := child.Rollback(ctx); rollbackErr != nil {
				return expiryResult{}, rollbackErr
			}
			result.failed.Count++
			if result.failed.first == nil {
				result.failed.first = err
			}
			continue
		}
		if err = child.Commit(ctx); err != nil {
			return expiryResult{}, err
		}
		result.removed++
	}
	if err = tx.Commit(ctx); err != nil {
		return expiryResult{}, err
	}
	return result, nil
}
