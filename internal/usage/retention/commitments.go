package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const commitmentBatch = 512

const refireScheduleTriggerSQL = `WITH batch AS (
 SELECT f.ctid FROM olp.attempt_usage_facts f WHERE f.commitment_schedule_id IS NULL
  AND EXISTS(SELECT 1 FROM olp.commitment_schedules s
   WHERE s.provider_id=f.provider_id AND f.observed_at>=olp.commitment_bucket(s.period,$1::timestamptz) AND s.effective_at<=f.observed_at AND (s.ends_at IS NULL OR s.ends_at>f.observed_at))
 ORDER BY f.observed_at LIMIT $2 FOR UPDATE SKIP LOCKED)
 UPDATE olp.attempt_usage_facts f SET commitment_weight=f.commitment_weight FROM batch WHERE f.ctid=batch.ctid`

func MaintainCommitments(ctx context.Context, db execer, now, retained time.Time) error {
	steps := []struct {
		name string
		sql  string
		args []any
	}{
		{"materialize commitments", `SELECT olp.materialize_commitments($1,$2,$3)`, []any{now, retained, commitmentBatch}},
		{"attach commitment schedules", refireScheduleTriggerSQL, []any{retained, commitmentBatch}},
		{"backfill commitment hours", `SELECT olp.backfill_commitment_hours($1,$2)`, []any{commitmentBatch, retained}},
	}
	for _, step := range steps {
		if _, err := db.Exec(ctx, step.sql, step.args...); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	return nil
}

type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}
