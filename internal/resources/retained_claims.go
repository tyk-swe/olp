package resources

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/platform/workerhealth"
)

var ErrSettlementClosed = errors.New("retained settlement is closed")

type RetainedClaim struct {
	ResourceID       uuid.UUID
	ClaimID          uuid.UUID
	Operation        string
	Hint             string
	RemoteTerminated bool
	TerminalAt       *time.Time
}

type SettlementStore struct {
	pool *pgxpool.Pool
}

func NewSettlementStore(pool *pgxpool.Pool) *SettlementStore {
	return &SettlementStore{pool: pool}
}

func (s *Store) Settlements() *SettlementStore {
	return &SettlementStore{pool: s.pool}
}

func (s *SettlementStore) Claim(ctx context.Context, limit int) ([]RetainedClaim, error) {
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

func (s *SettlementStore) Resource(ctx context.Context, c RetainedClaim) (*Resource, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM olp.provider_resources WHERE id=$1`, c.ResourceID))
}

func (s *SettlementStore) Heartbeat(ctx context.Context, c RetainedClaim) error {
	tag, err := s.pool.Exec(ctx, `UPDATE olp.retained_settlements SET lease_until=now()+interval '2 minutes' WHERE resource_id=$1 AND claim_id=$2 AND lease_until>now()`, c.ResourceID, c.ClaimID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrSubmissionConflict
	}
	return nil
}

func (s *SettlementStore) FinishClaim(ctx context.Context, c RetainedClaim, errorClass string) error {
	_, err := s.pool.Exec(ctx, `UPDATE olp.retained_settlements SET claim_id=NULL,lease_until=NULL,poll_after=now()+CASE WHEN $3='' THEN interval '60 seconds' ELSE LEAST(interval '1 hour',interval '60 seconds'*power(2,LEAST(failures,6))) END,failures=CASE WHEN $3='' THEN 0 ELSE failures+1 END,last_error=NULLIF($3,''),state=CASE WHEN state='closed' THEN state WHEN $3<>'' THEN 'evidence_missing' ELSE state END,updated_at=now() WHERE resource_id=$1 AND claim_id=$2`, c.ResourceID, c.ClaimID, errorClass)
	return err
}

func (s *SettlementStore) RequestReconciliationTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, hint string) error {
	if len(hint) > 512 {
		return ErrContract
	}
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM olp.retained_settlements WHERE resource_id=$1 FOR UPDATE`, id).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state == "closed" {
		return ErrSettlementClosed
	}
	_, updateErr := tx.Exec(ctx, `UPDATE olp.retained_settlements SET poll_after=now(),reconciliation_hint=NULLIF($2,''),updated_at=now() WHERE resource_id=$1`, id, hint)
	return updateErr
}

func (s *SettlementStore) CommitPoll(ctx context.Context, c RetainedClaim, r *Resource, observation RetainedObservation, adopt bool) error {
	if r == nil {
		return ErrContract
	}
	committedErr, err := withTxValue(ctx, s.pool, func(tx pgx.Tx) (error, error) {
		if err := commitPollResourceTx(ctx, tx, c, r, adopt); err != nil {
			return nil, err
		}
		if len(observation.Lines) == 0 && !observation.RemoteTerminated {
			return nil, nil
		}
		digest, err := retainedObservationDigest(observation)
		if err != nil {
			return nil, err
		}
		sub := EvidenceSubmission{
			ResourceID:  r.UUID,
			Key:         "provider:" + digest,
			Digest:      digest,
			Provenance:  "provider_verified",
			Observation: observation,
			Claim:       &c.ClaimID,
		}
		_, err = s.ApplyEvidenceTx(ctx, tx, sub)
		if errors.Is(err, ErrEvidenceConflict) && observation.RemoteTerminated {
			return err, nil
		}
		return nil, err
	})
	if err != nil {
		return err
	}
	return committedErr
}

func commitPollResourceTx(ctx context.Context, tx pgx.Tx, c RetainedClaim, r *Resource, adopt bool) error {
	var active bool
	err := tx.QueryRow(ctx, `SELECT true FROM olp.retained_settlements WHERE resource_id=$1 AND claim_id=$2 AND lease_until>now() FOR UPDATE`, c.ResourceID, c.ClaimID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSubmissionConflict
	}
	if err != nil {
		return err
	}
	if adopt {
		tag, adoptErr := tx.Exec(ctx, `UPDATE olp.retained_submissions SET state='accepted',updated_at=now() WHERE resource_id=$1 AND state IN ('dispatched','submission_unknown')`, r.UUID)
		if adoptErr != nil {
			return adoptErr
		}
		if tag.RowsAffected() != 1 {
			return ErrSubmissionConflict
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE olp.provider_resources SET upstream_id=CASE WHEN $5 THEN $6 ELSE upstream_id END,state=CASE WHEN state='deleted' THEN state ELSE $2 END,metadata=metadata||$3::jsonb,expires_at=$4,updated_at=clock_timestamp() WHERE id=$1 AND updated_at=$7 AND mutation_version=$8`, r.UUID, r.State, r.Metadata, r.ExpiresAt, adopt, r.UpstreamID, r.UpdatedAt, r.MutationVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrSubmissionConflict
	}
	return nil
}

func (s *SettlementStore) HasUnresolvedMutations(ctx context.Context, id uuid.UUID) (bool, error) {
	var pending bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.retained_mutations WHERE resource_id=$1 AND state IN ('dispatched','submission_unknown'))`, id).Scan(&pending)
	return pending, err
}

func (s *SettlementStore) Checkpoint(ctx context.Context, outcome workerhealth.Outcome) error {
	return workerhealth.New(workerhealth.TaskRetainedSettlement, "").Checkpoint(ctx, s.pool, outcome, false)
}

// CloseUndispatched uses the durable submission fence as proof that no outbound
// paid mutation occurred and never redispatches a prepared intent.
func (s *SettlementStore) CloseUndispatched(ctx context.Context, c RetainedClaim) (bool, error) {
	return withTxValue(ctx, s.pool, func(tx pgx.Tx) (bool, error) {
		var active bool
		err := tx.QueryRow(ctx, `SELECT true FROM olp.retained_settlements WHERE resource_id=$1 AND claim_id=$2 AND lease_until>clock_timestamp() FOR UPDATE`, c.ResourceID, c.ClaimID).Scan(&active)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrSubmissionConflict
		}
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
		return true, nil
	})
}
