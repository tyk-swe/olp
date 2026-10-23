package resources

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrSubmissionConflict = errors.New("retained submission identity conflicts")

// LookupLifecycle resolves the caller's original submission before any fresh
// target selection. The stored digest describes caller input, not its binding.
func (s *Store) LookupLifecycle(ctx context.Context, owner, operation, key string, digest []byte) (*Resource, error) {
	return lookupLifecycle(ctx, s.pool, owner, operation, key, digest)
}

func lookupLifecycle(ctx context.Context, query resourceRowQuerier, owner, operation, key string, digest []byte) (*Resource, error) {
	if len(key) < 1 || len(key) > 128 || len(digest) != 32 {
		return nil, ErrSubmissionConflict
	}
	var id uuid.UUID
	var priorDigest []byte
	err := query.QueryRow(ctx, `SELECT resource_id,request_digest FROM olp.retained_submissions WHERE api_key_id=$1 AND operation=$2 AND submission_key=$3`, owner, operation, key).Scan(&id, &priorDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(priorDigest, digest) {
		return nil, ErrSubmissionConflict
	}
	return scan(query.QueryRow(ctx, `SELECT `+columns+` FROM olp.provider_resources WHERE id=$1 AND api_key_id=$2`, id, owner))
}

// PrepareLifecycle atomically creates the owner mapping and submission fence.
// A replay can inspect its original resource, but never acquires dispatch rights.
func (s *Store) PrepareLifecycle(ctx context.Context, resource *Resource, operation, key string, digest []byte) (*Resource, bool, error) {
	if len(key) < 1 || len(key) > 128 || len(digest) != 32 {
		return nil, false, ErrSubmissionConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	// The same caller-supplied key may race on different gateway replicas.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, resource.APIKeyID+":"+operation+":"+key); err != nil {
		return nil, false, err
	}
	prior, err := lookupLifecycle(ctx, tx, resource.APIKeyID, operation, key, digest)
	if err == nil {
		return prior, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	if resource.UUID == uuid.Nil {
		resource.UUID = uuid.Must(uuid.NewV7())
	}
	resource.UpstreamID, resource.State = "pending:"+resource.UUID.String(), "submitting"
	out, err := s.PutTx(ctx, tx, resource)
	if err != nil {
		return nil, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO olp.retained_submissions(resource_id,api_key_id,operation,submission_key,request_digest,state) VALUES($1,$2,$3,$4,$5,'prepared')`, out.UUID, out.APIKeyID, operation, key, digest)
	if err != nil {
		return nil, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func (s *Store) DispatchLifecycle(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `UPDATE olp.retained_submissions SET state='dispatched',updated_at=now() WHERE resource_id=$1 AND state='prepared'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrSubmissionConflict
	}
	return nil
}

func (s *Store) UnknownLifecycle(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `WITH changed AS (UPDATE olp.retained_submissions SET state='submission_unknown',updated_at=now() WHERE resource_id=$1 AND state='dispatched' RETURNING resource_id) UPDATE olp.provider_resources SET state='submission_unknown',updated_at=now() WHERE id IN (SELECT resource_id FROM changed)`, id)
	return err
}

func (s *Store) CompleteLifecycle(ctx context.Context, r *Resource) error {
	if r.UpstreamID == "" || len(r.UpstreamID) > 512 || len(r.Metadata) > metadataLimit {
		return ErrContract
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE olp.retained_submissions SET state='accepted',updated_at=now() WHERE resource_id=$1 AND state IN ('dispatched','submission_unknown')`, r.UUID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrSubmissionConflict
	}
	_, err = tx.Exec(ctx, `UPDATE olp.provider_resources SET upstream_id=$2,state=$3,metadata=$4,expires_at=$5,updated_at=now() WHERE id=$1`, r.UUID, r.UpstreamID, r.State, r.Metadata, r.ExpiresAt)
	if err != nil {
		return fmt.Errorf("complete retained resource: %w", err)
	}
	return tx.Commit(ctx)
}

// Child checks both the child's owner and its immutable parent relationship.
func (s *Store) Child(ctx context.Context, kind, owner, id string, parent *Resource) (*Resource, error) {
	r, err := s.Get(ctx, kind, owner, id)
	if err != nil {
		return nil, err
	}
	if parent == nil || r.ParentID == nil || *r.ParentID != parent.UUID {
		return nil, ErrNotFound
	}
	return r, nil
}
