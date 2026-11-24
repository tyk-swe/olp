package apikeys

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/keyauthority"
)

const observationInterval = time.Second
const observationWriteTimeout = time.Second

type overlapID struct {
	KeyID    string `json:"key_id"`
	LookupID string `json:"lookup_id"`
}

type overlapObservation struct {
	overlapID
	At        time.Time `json:"at"`
	expiresAt time.Time
}

type OverlapRecorder struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	mu   sync.Mutex
	// Coalesce requests by overlap so a busy key cannot fill a request queue.
	pending map[overlapID]overlapObservation
	failed  map[overlapID]overlapObservation
}

func NewOverlapRecorder(pool *pgxpool.Pool, log *slog.Logger) *OverlapRecorder {
	return &OverlapRecorder{pool: pool, log: log, pending: make(map[overlapID]overlapObservation), failed: make(map[overlapID]overlapObservation)}
}

func (r *OverlapRecorder) Observe(keyID string, overlap keyauthority.SecretOverlap, at time.Time) {
	id := overlapID{KeyID: keyID, LookupID: overlap.LookupID}
	r.mu.Lock()
	defer r.mu.Unlock()
	keepLatest(r.pending, overlapObservation{overlapID: id, At: at, expiresAt: overlap.ExpiresAt})
}

func keepLatest(dst map[overlapID]overlapObservation, item overlapObservation) {
	if item.At.After(dst[item.overlapID].At) {
		dst[item.overlapID] = item
	}
}

func (r *OverlapRecorder) Run(ctx context.Context) {
	ticker := time.NewTicker(observationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Flush(ctx)
		}
	}
}

const updateActiveOverlaps = `
 FROM jsonb_to_recordset($1::jsonb) AS sample(key_id uuid, lookup_id text, at timestamptz)
 JOIN olp.api_keys k ON k.id=sample.key_id
 WHERE o.api_key_id=k.id AND o.lookup_id=sample.lookup_id AND k.revoked_at IS NULL
 AND LEAST(o.expires_at,k.expires_at)>clock_timestamp() AND sample.at<LEAST(o.expires_at,k.expires_at)`

const recordOverlapUse = `UPDATE olp.api_key_overlaps o
 SET last_observed_use=sample.at` + updateActiveOverlaps +
	` AND (o.last_observed_use IS NULL OR o.last_observed_use<sample.at)`

const recordOverlapFailure = `UPDATE olp.api_key_overlaps o
 SET last_recording_error_at=sample.at` + updateActiveOverlaps +
	` AND (o.last_recording_error_at IS NULL OR o.last_recording_error_at<sample.at)`

// Flush runs only on the recorder loop or after it has stopped for shutdown.
func (r *OverlapRecorder) Flush(ctx context.Context) {
	pending := r.takePending()
	if err := r.write(ctx, recordOverlapUse, pending); err != nil {
		r.log.Warn("API key overlap observation write failed", "cause", overlapWriteCause(err))
		r.markFailed(pending)
	}
	failed := r.failureBatch()
	if err := r.write(ctx, recordOverlapFailure, failed); err != nil {
		r.log.Warn("API key overlap recording health write failed", "cause", overlapWriteCause(err))
		return
	}
	r.mu.Lock()
	clear(r.failed)
	r.mu.Unlock()
}

func (r *OverlapRecorder) takePending() map[overlapID]overlapObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := r.pending
	r.pending = make(map[overlapID]overlapObservation)
	return pending
}

func (r *OverlapRecorder) markFailed(pending map[overlapID]overlapObservation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range pending {
		keepLatest(r.failed, item)
	}
}

func (r *OverlapRecorder) failureBatch() map[overlapID]overlapObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	maps.DeleteFunc(r.failed, func(_ overlapID, item overlapObservation) bool { return !item.expiresAt.After(now) })
	return maps.Clone(r.failed)
}

func (r *OverlapRecorder) write(ctx context.Context, statement string, samples map[overlapID]overlapObservation) error {
	if len(samples) == 0 {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(ctx, observationWriteTimeout)
	defer cancel()
	_, err := r.pool.Exec(writeCtx, statement, slices.Collect(maps.Values(samples)))
	return err
}

func overlapWriteCause(err error) string {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &pgErr):
		return pgErr.Code
	default:
		return "store_error"
	}
}

func (r *OverlapRecorder) pendingFailures(keyID string) map[string]time.Time {
	failures := make(map[string]time.Time)
	if r == nil {
		return failures
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, item := range r.failed {
		if id.KeyID == keyID {
			failures[id.LookupID] = item.At
		}
	}
	return failures
}
