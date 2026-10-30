package limits

import (
	"context"
	"time"
)

const WorkflowModelLeaseDuration = 12 * time.Second

func (s *DebitStore) MarkWorkflowModelDispatched(ctx context.Context, id, claim string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := MarkDispatchedTx(ctx, tx, id, claim); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO olp.workflow_model_leases(dispatch_id,claim_id,expires_at)
		SELECT id,claim_id,clock_timestamp()+make_interval(secs=>$3)
		FROM olp.execution_debits WHERE id=$1 AND claim_id=$2 AND kind='model' AND run_id IS NOT NULL`, id, claim, WorkflowModelLeaseDuration.Seconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrDebitState
	}
	return tx.Commit(ctx)
}

func (s *DebitStore) RenewWorkflowModelLease(ctx context.Context, id, claim string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var active bool
	err = tx.QueryRow(ctx, `SELECT l.expires_at>clock_timestamp()
		FROM olp.execution_debits d JOIN olp.workflow_model_leases l ON l.dispatch_id=d.id
		WHERE d.id=$1 AND d.claim_id=$2 AND l.claim_id=$2 FOR UPDATE OF d,l`, id, claim).Scan(&active)
	if err != nil {
		return err
	}
	if !active {
		return ErrDebitState
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.workflow_model_leases SET expires_at=clock_timestamp()+make_interval(secs=>$3)
		WHERE dispatch_id=$1 AND claim_id=$2`, id, claim, WorkflowModelLeaseDuration.Seconds()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *DebitStore) RecoverOrphanedWorkflowModels(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `WITH expired AS (
		SELECT d.id FROM olp.execution_debits d
		JOIN olp.workflow_model_leases l ON l.dispatch_id=d.id AND l.claim_id=d.claim_id
		WHERE d.kind='model' AND d.state='dispatched' AND l.expires_at<=clock_timestamp()
		ORDER BY l.expires_at,d.id FOR UPDATE OF d,l SKIP LOCKED LIMIT $1
	) UPDATE olp.execution_debits d SET state='outcome_unknown'
		FROM expired WHERE d.id=expired.id AND d.state='dispatched'`, limit)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
