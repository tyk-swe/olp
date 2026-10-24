package resources

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/usage"
)

type RetainedClaim struct {
	ResourceID       uuid.UUID
	ClaimID          uuid.UUID
	Operation        string
	Hint             string
	RemoteTerminated bool
	TerminalAt       *time.Time
}

func (s *Store) ClaimRetained(ctx context.Context, limit int) ([]RetainedClaim, error) {
	if limit < 1 || limit > 16 {
		return nil, ErrContract
	}
	rows, err := s.pool.Query(ctx, `WITH due AS (SELECT resource_id FROM olp.retained_settlements WHERE state<>'closed' AND poll_after<=now() AND (lease_until IS NULL OR lease_until<now()) ORDER BY poll_after,resource_id LIMIT $1 FOR UPDATE SKIP LOCKED) UPDATE olp.retained_settlements s SET claim_id=gen_random_uuid(),lease_until=now()+interval '2 minutes' FROM due WHERE s.resource_id=due.resource_id RETURNING s.resource_id,s.claim_id,s.operation,COALESCE(s.reconciliation_hint,''),s.remote_terminated,s.terminal_at`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RetainedClaim
	for rows.Next() {
		var c RetainedClaim
		if err = rows.Scan(&c.ResourceID, &c.ClaimID, &c.Operation, &c.Hint, &c.RemoteTerminated, &c.TerminalAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) RetainedClaimResource(ctx context.Context, c RetainedClaim) (*Resource, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM olp.provider_resources WHERE id=$1`, c.ResourceID))
}
func (s *Store) HeartbeatRetained(ctx context.Context, c RetainedClaim) error {
	tag, err := s.pool.Exec(ctx, `UPDATE olp.retained_settlements SET lease_until=now()+interval '2 minutes' WHERE resource_id=$1 AND claim_id=$2 AND lease_until>now()`, c.ResourceID, c.ClaimID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrSubmissionConflict
	}
	return nil
}
func (s *Store) FinishRetainedClaim(ctx context.Context, c RetainedClaim, errorClass string) error {
	_, err := s.pool.Exec(ctx, `UPDATE olp.retained_settlements SET claim_id=NULL,lease_until=NULL,poll_after=now()+CASE WHEN $3='' THEN interval '60 seconds' ELSE LEAST(interval '1 hour',interval '60 seconds'*power(2,LEAST(failures,6))) END,failures=CASE WHEN $3='' THEN 0 ELSE failures+1 END,last_error=NULLIF($3,''),state=CASE WHEN state='closed' THEN state WHEN $3<>'' THEN 'evidence_missing' ELSE state END,updated_at=now() WHERE resource_id=$1 AND claim_id=$2`, c.ResourceID, c.ClaimID, errorClass)
	return err
}
func (s *Store) RequestRetainedReconciliationTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, hint string) error {
	if len(hint) > 512 {
		return ErrContract
	}
	tag, err := tx.Exec(ctx, `UPDATE olp.retained_settlements SET poll_after=now(),reconciliation_hint=NULLIF($2,''),updated_at=now() WHERE resource_id=$1`, id, hint)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}
func (s *Store) CommitRetainedPoll(ctx context.Context, c RetainedClaim, r *Resource, observation RetainedObservation, adopt bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var active bool
	err = tx.QueryRow(ctx, `SELECT true FROM olp.retained_settlements WHERE resource_id=$1 AND claim_id=$2 AND lease_until>now() FOR UPDATE`, c.ResourceID, c.ClaimID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSubmissionConflict
	}
	if err != nil {
		return err
	}
	if adopt {
		tag, e := tx.Exec(ctx, `UPDATE olp.retained_submissions SET state='accepted',updated_at=now() WHERE resource_id=$1 AND state IN ('dispatched','submission_unknown')`, r.UUID)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return ErrSubmissionConflict
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE olp.provider_resources SET upstream_id=CASE WHEN $5 THEN $6 ELSE upstream_id END,state=CASE WHEN state='deleted' THEN state ELSE $2 END,metadata=metadata||$3::jsonb,expires_at=COALESCE($4,expires_at),updated_at=clock_timestamp() WHERE id=$1 AND updated_at=$7`, r.UUID, r.State, r.Metadata, r.ExpiresAt, adopt, r.UpstreamID, r.UpdatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrSubmissionConflict
	}
	if len(observation.Lines) > 0 || observation.RemoteTerminated {
		key := retainedEvidenceKey(observation)
		if _, err = s.ApplyRetainedEvidenceTx(ctx, tx, r.UUID, key, retainedObservationDigest(observation), "provider_verified", "", observation, &c.ClaimID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func (s *Store) RetainedEvidenceTerminationTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (bool, *time.Time, error) {
	var term bool
	var at *time.Time
	err := tx.QueryRow(ctx, `SELECT remote_terminated,terminal_at FROM olp.retained_settlements WHERE resource_id=$1 FOR UPDATE`, id).Scan(&term, &at)
	return term, at, err
}
func (s *Store) RetainedTemplates(ctx context.Context, id uuid.UUID) (map[string]*usage.Event, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT templates FROM olp.retained_settlements WHERE resource_id=$1`, id).Scan(&raw)
	if err != nil {
		return nil, err
	}
	var out map[string]*usage.Event
	err = json.Unmarshal(raw, &out)
	return out, err
}

func (s *Store) CheckpointRetained(ctx context.Context, outcome usage.Outcome, progress bool) error {
	return usage.CheckpointTask(ctx, s.pool, usage.TaskRetainedSettlement, outcome, progress)
}

// CloseUndispatchedRetained uses the durable submission fence as proof that no
// outbound paid mutation occurred. It never redispatches a prepared intent.
func (s *Store) CloseUndispatchedRetained(ctx context.Context, c RetainedClaim) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var active bool
	err = tx.QueryRow(ctx, `SELECT true FROM olp.retained_settlements WHERE resource_id=$1 AND claim_id=$2 AND lease_until>clock_timestamp() FOR UPDATE`, c.ResourceID, c.ClaimID).Scan(&active)
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE olp.retained_submissions SET state='not_accepted',updated_at=now() WHERE resource_id=$1 AND state='prepared' AND created_at<now()-interval '2 minutes'`, c.ResourceID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err = tx.Exec(ctx, `UPDATE olp.provider_resources SET state='not_accepted',updated_at=clock_timestamp() WHERE id=$1`, c.ResourceID); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE olp.retained_settlements SET state='closed',updated_at=clock_timestamp() WHERE resource_id=$1`, c.ResourceID); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
