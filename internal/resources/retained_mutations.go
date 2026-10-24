package resources

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A dispatched paid mutation never acquires dispatch rights again, even if its
// caller or process disappeared before acceptance could be recorded.
func (s *Store) PrepareRetainedMutation(ctx context.Context, r *Resource, operation, key string, digest []byte) error {
	if len(key) < 1 || len(key) > 128 || len(digest) != 32 {
		return ErrSubmissionConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, r.APIKeyID+":"+operation+":"+key); err != nil {
		return err
	}
	// Serialize new paid work with settlement's final coverage check.
	var settlementState string
	err = tx.QueryRow(ctx, `SELECT state FROM olp.retained_settlements WHERE resource_id=$1 FOR UPDATE`, r.UUID).Scan(&settlementState)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if settlementState == "closed" {
		return ErrSubmissionConflict
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.retained_mutations WHERE api_key_id=$1 AND operation=$2 AND submission_key=$3)`, r.APIKeyID, operation, key).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrSubmissionConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO olp.retained_mutations(api_key_id,operation,submission_key,resource_id,request_digest,state) VALUES($1,$2,$3,$4,$5,'dispatched')`, r.APIKeyID, operation, key, r.UUID, digest)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) UnknownRetainedMutation(ctx context.Context, id uuid.UUID, key, operation string) error {
	_, err := s.pool.Exec(ctx, `UPDATE olp.retained_mutations SET state='submission_unknown',updated_at=now() WHERE resource_id=$1 AND submission_key=$2 AND operation=$3 AND state='dispatched'`, id, key, operation)
	return err
}
func (s *Store) AcceptRetainedMutation(ctx context.Context, id uuid.UUID, key, operation string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE olp.retained_mutations SET state='accepted',updated_at=now() WHERE resource_id=$1 AND submission_key=$2 AND operation=$3 AND state='dispatched'`, id, key, operation)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrSubmissionConflict
	}
	return nil
}

// SaveRetainedResult fences read/TTL observations by the version used for their
// upstream request. A delayed response cannot overwrite a newer accepted expiry.
func (s *Store) SaveRetainedResult(ctx context.Context, r *Resource) error {
	if r == nil || len(r.Metadata) > metadataLimit {
		return ErrContract
	}
	var updated time.Time
	err := s.pool.QueryRow(ctx, `UPDATE olp.provider_resources SET metadata=metadata||$2::jsonb,expires_at=$3,state=$5,updated_at=clock_timestamp() WHERE id=$1 AND updated_at=$4 AND state<>'deleted' RETURNING updated_at`, r.UUID, r.Metadata, r.ExpiresAt, r.UpdatedAt, r.State).Scan(&updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSubmissionConflict
	}
	if err != nil {
		return err
	}
	r.UpdatedAt = updated
	return nil
}

func (s *Store) HasUnresolvedRetainedMutations(ctx context.Context, id uuid.UUID) (bool, error) {
	var pending bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.retained_mutations WHERE resource_id=$1 AND state IN ('dispatched','submission_unknown'))`, id).Scan(&pending)
	return pending, err
}

// RejectRetained records definitive transport/provider non-acceptance, never a
// timeout or an ambiguous server failure. Only the original dispatch owner calls
// it, before returning the provider's rejection to the client.
func (s *Store) RejectRetained(ctx context.Context, r *Resource, key, operation string, create bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if create {
		tag, e := tx.Exec(ctx, `UPDATE olp.retained_submissions SET state='not_accepted',updated_at=now() WHERE resource_id=$1 AND state='dispatched'`, r.UUID)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return ErrSubmissionConflict
		}
		if _, err = tx.Exec(ctx, `UPDATE olp.provider_resources SET state='not_accepted',updated_at=clock_timestamp() WHERE id=$1`, r.UUID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE olp.retained_settlements SET state='closed',updated_at=clock_timestamp() WHERE resource_id=$1`, r.UUID); err != nil {
			return err
		}
	} else {
		if _, err = tx.Exec(ctx, `UPDATE olp.retained_mutations SET state='not_accepted',updated_at=clock_timestamp() WHERE resource_id=$1 AND submission_key=$2 AND operation=$3 AND state='dispatched'`, r.UUID, key, operation); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
