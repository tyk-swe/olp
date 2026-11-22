package batches

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
)

type recordOutcome int

const (
	recordedNew recordOutcome = iota
	recordedDuplicate
	recordedConflict
)

func recordResultTx(ctx context.Context, tx pgx.Tx, settlement Settlement, digest []byte, result Result, now time.Time) (recordOutcome, []limits.Snapshot, error) {
	evidence := EvidenceDigest(result.Evidence)
	var existing []byte
	err := tx.QueryRow(ctx, `SELECT evidence_digest FROM olp.batch_settlement_results
		WHERE resource_id=$1 AND custom_id_digest=$2 AND result_kind=$3 FOR UPDATE`,
		settlement.ResourceID, digest, result.Kind).Scan(&existing)
	if err == nil && bytes.Equal(existing, evidence) {
		return recordedDuplicate, nil, nil
	}
	if err == nil {
		return recordedConflict, nil, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, fmt.Errorf("read batch result identity: %w", err)
	}
	if result.Kind != ResultGap {
		var otherKind string
		err = tx.QueryRow(ctx, `SELECT result_kind FROM olp.batch_settlement_results
			WHERE resource_id=$1 AND custom_id_digest=$2 ORDER BY result_kind LIMIT 1 FOR UPDATE`,
			settlement.ResourceID, digest).Scan(&otherKind)
		if err == nil {
			return recordedConflict, nil, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, nil, fmt.Errorf("read batch result kind: %w", err)
		}
	}
	event, err := ResultEvent(settlement.Template, settlement.ResourceID, digest, result, now)
	if err != nil {
		return 0, nil, fmt.Errorf("build batch result event: %w", err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO olp.batch_settlement_results
		(resource_id,custom_id_digest,result_kind,event_id,evidence_digest)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
		settlement.ResourceID, digest, result.Kind, event.EventID, evidence)
	if err != nil {
		return 0, nil, fmt.Errorf("insert batch result identity: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return recordedDuplicate, nil, nil
	}
	accepted, err := usage.NewAccountingEvent(event)
	if err != nil {
		return 0, nil, err
	}
	persisted, err := ingest.PersistEventTx(ctx, tx, accepted)
	if err != nil {
		return 0, nil, fmt.Errorf("persist batch usage event: %w", err)
	}
	return recordedNew, persisted.CostSnapshots, nil
}

func (s *Store) CommitPage(ctx context.Context, settlement Settlement, results []PageResult, cursor Cursor, now time.Time) ([]limits.Snapshot, error) {
	if len(results) > PageSize {
		return nil, errors.New("batch settlement page is too large")
	}
	encodedCursor, err := json.Marshal(cursor)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin batch result page: %w", err)
	}
	defer tx.Rollback(ctx)
	var snapshots []limits.Snapshot
	var settled int64
	state := settlement.State
	for _, page := range results {
		if len(page.CustomIDDigest) == 0 || len(page.Result.Evidence) == 0 || page.Result.Kind == ResultGap {
			return nil, errors.New("batch page result is malformed")
		}
		outcome, persisted, err := recordResultTx(ctx, tx, settlement, page.CustomIDDigest, page.Result, now)
		if err != nil {
			return nil, err
		}
		if outcome == recordedConflict {
			state = StateConflict
			break
		}
		snapshots = append(snapshots, persisted...)
		if outcome == recordedNew {
			settled++
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE olp.batch_settlements
		SET state=$3,cursor=$4,settled_results=settled_results+$5,
		    attempts=CASE WHEN $5>0 THEN 0 ELSE attempts END,updated_at=$6
		WHERE resource_id=$1 AND claim_id=$2`,
		settlement.ResourceID, settlement.ClaimID, state, encodedCursor, settled, now)
	if err != nil {
		return nil, fmt.Errorf("commit batch settlement page: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, ErrClaimLost
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit batch settlement page: %w", err)
	}
	if state == StateConflict {
		return snapshots, ErrResultConflict
	}
	return snapshots, nil
}

func (s *Store) ResultCounts(ctx context.Context, resourceID uuid.UUID) (ResultCounts, error) {
	rows, err := s.pool.Query(ctx, `SELECT result_kind,count(*) FROM olp.batch_settlement_results
		WHERE resource_id=$1 GROUP BY result_kind`, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := ResultCounts{}
	for rows.Next() {
		var kind ResultKind
		var count int64
		if err = rows.Scan(&kind, &count); err != nil {
			return nil, err
		}
		counts[kind] = count
	}
	return counts, rows.Err()
}

func (s *Store) Finish(ctx context.Context, settlement Settlement, state State, next time.Time, errorClass ErrorClass) error {
	if state == StateSettled || state == StateUncertain {
		return errors.New("closing states finish through Close")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE olp.batch_settlements
		SET state=$3,next_attempt_at=$4,last_error_class=NULLIF($5,''),claim_id=NULL,
		    claimed_until=NULL,updated_at=now()
		WHERE resource_id=$1 AND claim_id=$2 AND (NOT $6::text=ANY($7) OR state=$6)`,
		settlement.ResourceID, settlement.ClaimID, state, next, errorClass.stored(), settlement.State, submissionStates)
	if err != nil {
		return fmt.Errorf("finish batch settlement: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}

func (s *Store) Close(ctx context.Context, settlement Settlement, closing Closing, now time.Time) ([]limits.Snapshot, error) {
	if closing.State != StateSettled && closing.State != StateUncertain {
		return nil, errors.New("batch settlements close as settled or uncertain")
	}
	if closing.Gap > 0 && len(closing.GapDigest) == 0 {
		return nil, errors.New("batch gap digest is empty")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin batch settlement close: %w", err)
	}
	defer tx.Rollback(ctx)
	snapshots, uncertain, err := recordGapTx(ctx, tx, settlement, closing, now)
	if err != nil {
		return nil, err
	}
	if closing.State == StateUncertain {
		if _, err = tx.Exec(ctx, `DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2
			AND NOT EXISTS(SELECT 1 FROM olp.provider_resources WHERE id=$1)`,
			settlement.ResourceID, secrets.ProviderContinuation); err != nil {
			return nil, fmt.Errorf("delete uncertain batch contract: %w", err)
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE olp.batch_settlements
		SET state=$3,closed_at=$4,next_attempt_at=$4,uncertain_results=uncertain_results+$5,
		    claim_id=NULL,claimed_until=NULL,last_error_class=NULLIF($6,''),updated_at=$4
		WHERE resource_id=$1 AND claim_id=$2`, settlement.ResourceID, settlement.ClaimID,
		closing.State, now, uncertain, closing.ErrorClass.stored())
	if err != nil {
		return nil, fmt.Errorf("close batch settlement: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, ErrClaimLost
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit batch settlement close: %w", err)
	}
	return snapshots, nil
}

func recordGapTx(ctx context.Context, tx pgx.Tx, settlement Settlement, closing Closing, now time.Time) ([]limits.Snapshot, int64, error) {
	if closing.Gap <= 0 {
		return nil, 0, nil
	}
	gap := Result{
		CustomID:   "*",
		Kind:       ResultGap,
		Usage:      usage.AttemptUsage{Evidence: usage.EvidenceUnobservedUncertain},
		ErrorClass: "billing_uncertain",
		Evidence:   fmt.Appendf(nil, "batch-gap:%d", closing.Gap),
	}
	outcome, snapshots, err := recordResultTx(ctx, tx, settlement, closing.GapDigest, gap, now)
	switch {
	case err != nil:
		return nil, 0, err
	case outcome == recordedConflict:
		return nil, 0, ErrResultConflict
	case outcome == recordedDuplicate:
		return nil, 0, nil
	default:
		return snapshots, closing.Gap, nil
	}
}
