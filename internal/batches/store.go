package batches

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

var (
	ErrSubmissionInFlight = errors.New("batch submission outcome unknown")
	ErrClaimLost          = errors.New("batch settlement claim lost")
)

const (
	claimLease = 2 * time.Minute
	purgeLimit = 50000
)

type Store struct {
	Pool *pgxpool.Pool
}

type PageResult struct {
	CustomIDDigest []byte
	Result         Result
	GapCount       int64
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{Pool: pool}
}

const settlementColumns = `resource_id,kind,adapter,api_key_id,route_slug,route_revision_id,provider_id,
	provider_revision_id,target_id,slot_id,credential_id,upstream_id,surface,submission_digest,line_operation,
	template,state,expected_results,settled_results,uncertain_results,cursor,claim_id,claimed_until,attempts,
	next_attempt_at,last_error_class,close_after,created_at,updated_at,closed_at`

func (s *Store) BeginSubmission(ctx context.Context, settlement *Settlement) error {
	if settlement == nil || settlement.ResourceID == uuid.Nil || settlement.Template == nil ||
		settlement.State != StateSubmitting || len(settlement.SubmissionDigest) == 0 {
		return errors.New("invalid batch submission intent")
	}
	template, err := usage.Encode(settlement.Template)
	if err != nil {
		return err
	}
	cursor := settlement.Cursor
	if len(cursor) == 0 {
		cursor = []byte(`{}`)
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO olp.batch_settlements
		(resource_id,kind,adapter,api_key_id,route_slug,route_revision_id,provider_id,provider_revision_id,
		 target_id,slot_id,credential_id,upstream_id,surface,submission_digest,line_operation,template,state,
		 expected_results,settled_results,uncertain_results,cursor,close_after)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,0,0,$19,$20)`,
		settlement.ResourceID, settlement.Kind, settlement.Adapter, settlement.APIKeyID, settlement.RouteSlug,
		settlement.RouteRevisionID, settlement.ProviderID, settlement.ProviderRevisionID, settlement.TargetID,
		settlement.SlotID, settlement.CredentialID, settlement.UpstreamID, settlement.Surface,
		settlement.SubmissionDigest, settlement.LineOperation, template, settlement.State,
		settlement.ExpectedResults, cursor, settlement.CloseAfter)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "batch_settlements_submission_identity" {
			return ErrSubmissionInFlight
		}
		return fmt.Errorf("insert batch submission intent: %w", err)
	}
	return nil
}

func (s *Store) DeleteSubmission(ctx context.Context, resourceID uuid.UUID) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin batch submission cleanup: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM olp.batch_settlements
		WHERE resource_id=$1 AND state IN ('submitting','submission_unknown')`, resourceID); err != nil {
		return fmt.Errorf("delete batch submission intent: %w", err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2`, resourceID, secrets.ProviderContinuation); err != nil {
		return fmt.Errorf("delete staged batch contract: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit batch submission cleanup: %w", err)
	}
	return nil
}

func (s *Store) MarkUnknown(ctx context.Context, resourceID uuid.UUID, errorClass string, next time.Time) error {
	if len(errorClass) > 120 {
		errorClass = errorClass[:120]
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE olp.batch_settlements
		SET state='submission_unknown',last_error_class=$2,next_attempt_at=$3,updated_at=now()
		WHERE resource_id=$1 AND state='submitting'`, resourceID, errorClass, next)
	if err != nil {
		return fmt.Errorf("mark batch submission unknown: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}

func (s *Store) ActivateTx(ctx context.Context, tx pgx.Tx, resourceID uuid.UUID, upstreamID string, expected *int64) error {
	tag, err := tx.Exec(ctx, `UPDATE olp.batch_settlements
		SET state='active',upstream_id=$2,expected_results=$3,next_attempt_at=now(),
		    last_error_class=NULL,updated_at=now()
		WHERE resource_id=$1 AND state IN ('submitting','submission_unknown')`, resourceID, upstreamID, expected)
	if err != nil {
		return fmt.Errorf("activate batch settlement: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}

func (s *Store) Claim(ctx context.Context, now time.Time, limit int) ([]Settlement, error) {
	if limit <= 0 {
		return nil, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim batch settlements: %w", err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT `+settlementColumns+`
		FROM olp.batch_settlements
		WHERE state IN ('submitting','submission_unknown','active','settling','conflict')
		  AND next_attempt_at<=$1 AND (claim_id IS NULL OR claimed_until<=$1)
		ORDER BY next_attempt_at,resource_id
		LIMIT $2 FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("claim batch settlements: %w", err)
	}
	var settlements []Settlement
	for rows.Next() {
		settlement, scanErr := scanSettlement(rows)
		if scanErr != nil {
			rows.Close()
			return nil, fmt.Errorf("scan batch settlement claim: %w", scanErr)
		}
		settlements = append(settlements, settlement)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read batch settlement claims: %w", err)
	}
	for i := range settlements {
		claimID := uuid.Must(uuid.NewV7())
		_, err = tx.Exec(ctx, `UPDATE olp.batch_settlements
			SET claim_id=$2,claimed_until=$3,attempts=attempts+1,updated_at=$4
			WHERE resource_id=$1`, settlements[i].ResourceID, claimID, now.Add(claimLease), now)
		if err != nil {
			return nil, fmt.Errorf("lease batch settlement: %w", err)
		}
		settlements[i].ClaimID = &claimID
		claimedUntil := now.Add(claimLease)
		settlements[i].ClaimedUntil = &claimedUntil
		settlements[i].Attempts++
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit batch settlement claims: %w", err)
	}
	return settlements, nil
}

func scanSettlement(row pgx.Row) (Settlement, error) {
	var settlement Settlement
	var kind, adapter, state string
	var template, cursor []byte
	var upstreamID, credentialID *string
	err := row.Scan(&settlement.ResourceID, &kind, &adapter, &settlement.APIKeyID, &settlement.RouteSlug,
		&settlement.RouteRevisionID, &settlement.ProviderID, &settlement.ProviderRevisionID, &settlement.TargetID,
		&settlement.SlotID, &credentialID, &upstreamID, &settlement.Surface, &settlement.SubmissionDigest,
		&settlement.LineOperation, &template, &state, &settlement.ExpectedResults, &settlement.SettledResults,
		&settlement.UncertainResults, &cursor, &settlement.ClaimID, &settlement.ClaimedUntil,
		&settlement.Attempts, &settlement.NextAttemptAt, &settlement.LastErrorClass, &settlement.CloseAfter,
		&settlement.CreatedAt, &settlement.UpdatedAt, &settlement.ClosedAt)
	if err != nil {
		return Settlement{}, err
	}
	settlement.Kind, settlement.Adapter, settlement.State = kind, adapter, State(state)
	settlement.CredentialID, settlement.UpstreamID = credentialID, upstreamID
	settlement.Cursor = cursor
	settlement.Template, err = usage.Decode(template)
	if err != nil {
		return Settlement{}, err
	}
	return settlement, nil
}

func (s *Store) ExtendClaim(ctx context.Context, settlement Settlement, now time.Time) error {
	if settlement.ClaimID == nil {
		return ErrClaimLost
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE olp.batch_settlements
		SET claimed_until=$3,updated_at=$4 WHERE resource_id=$1 AND claim_id=$2
		  AND state IN ('submitting','submission_unknown','active','settling','conflict')`,
		settlement.ResourceID, *settlement.ClaimID, now.Add(claimLease), now)
	if err != nil {
		return fmt.Errorf("extend batch settlement claim: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}

func (s *Store) SetExpected(ctx context.Context, settlement Settlement, expected *int64) error {
	if settlement.ClaimID == nil {
		return ErrClaimLost
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE olp.batch_settlements SET expected_results=$3,updated_at=now()
		WHERE resource_id=$1 AND claim_id=$2`, settlement.ResourceID, *settlement.ClaimID, expected)
	if err != nil {
		return fmt.Errorf("update batch expected result count: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}

func (s *Store) CommitPage(ctx context.Context, settlement Settlement, results []PageResult, cursor any, now time.Time) ([]limits.CostSnapshot, ResultCounts, bool, error) {
	if settlement.ClaimID == nil || len(results) > 100 {
		return nil, nil, false, errors.New("invalid batch settlement page")
	}
	cursorBytes, err := checkpointCursor(settlement.Cursor, cursor)
	if err != nil {
		return nil, nil, false, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, nil, false, fmt.Errorf("begin batch result page: %w", err)
	}
	defer tx.Rollback(ctx)
	var snapshots []limits.CostSnapshot
	var settledDelta, uncertainDelta int64
	conflict := false
	for _, pageResult := range results {
		if len(pageResult.CustomIDDigest) == 0 || len(pageResult.Result.Evidence) == 0 {
			return nil, nil, false, errors.New("batch result identity or evidence is empty")
		}
		var existingEvidence []byte
		err = tx.QueryRow(ctx, `SELECT evidence_digest FROM olp.batch_settlement_results
			WHERE resource_id=$1 AND custom_id_digest=$2 AND result_kind=$3 FOR UPDATE`,
			settlement.ResourceID, pageResult.CustomIDDigest, pageResult.Result.Kind).Scan(&existingEvidence)
		if err == nil {
			if !equalBytes(existingEvidence, EvidenceDigest(pageResult.Result.Evidence)) {
				conflict = true
				break
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, false, fmt.Errorf("read batch result identity: %w", err)
		}
		var otherKind string
		err = tx.QueryRow(ctx, `SELECT result_kind FROM olp.batch_settlement_results
			WHERE resource_id=$1 AND custom_id_digest=$2 ORDER BY result_kind LIMIT 1 FOR UPDATE`,
			settlement.ResourceID, pageResult.CustomIDDigest).Scan(&otherKind)
		if err == nil {
			conflict = true
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, false, fmt.Errorf("read batch result kind: %w", err)
		}
		event, err := ResultEvent(settlement.Template, settlement.ResourceID, pageResult.CustomIDDigest, pageResult.Result, now)
		if err != nil {
			return nil, nil, false, fmt.Errorf("build batch result event: %w", err)
		}
		eventID, err := uuid.Parse(event.EventID)
		if err != nil {
			return nil, nil, false, err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO olp.batch_settlement_results
			(resource_id,custom_id_digest,result_kind,event_id,evidence_digest)
			VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
			settlement.ResourceID, pageResult.CustomIDDigest, pageResult.Result.Kind, eventID,
			EvidenceDigest(pageResult.Result.Evidence))
		if err != nil {
			return nil, nil, false, fmt.Errorf("insert batch result identity: %w", err)
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		payload, err := usage.Encode(event)
		if err != nil {
			return nil, nil, false, err
		}
		persisted, err := usage.PersistEventTx(ctx, tx, event, payload)
		if err != nil {
			return nil, nil, false, fmt.Errorf("persist batch usage event: %w", err)
		}
		snapshots = append(snapshots, persisted.CostSnapshots...)
		if pageResult.Result.Kind == ResultGap {
			gap := pageResult.GapCount
			if gap < 1 {
				gap = 1
			}
			uncertainDelta += gap
		} else {
			settledDelta++
		}
	}
	state := settlement.State
	if conflict {
		state = StateConflict
	}
	tag, err := tx.Exec(ctx, `UPDATE olp.batch_settlements
		SET state=$3,cursor=$4,settled_results=settled_results+$5,
		    uncertain_results=uncertain_results+$6,
		    attempts=CASE WHEN $5>0 THEN 0 ELSE attempts END,updated_at=$7
		WHERE resource_id=$1 AND claim_id=$2`,
		settlement.ResourceID, *settlement.ClaimID, state, cursorBytes, settledDelta, uncertainDelta, now)
	if err != nil {
		return nil, nil, false, fmt.Errorf("commit batch settlement page: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, nil, false, ErrClaimLost
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, nil, false, fmt.Errorf("commit batch settlement page: %w", err)
	}
	counts, err := s.ResultCounts(ctx, settlement.ResourceID)
	if err != nil {
		return nil, nil, false, err
	}
	return snapshots, counts, conflict, nil
}

func (s *Store) ResultCounts(ctx context.Context, resourceID uuid.UUID) (ResultCounts, error) {
	rows, err := s.Pool.Query(ctx, `SELECT result_kind,count(*) FROM olp.batch_settlement_results
		WHERE resource_id=$1 GROUP BY result_kind`, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := ResultCounts{}
	for rows.Next() {
		var kind string
		var count int64
		if err = rows.Scan(&kind, &count); err != nil {
			return nil, err
		}
		counts[ResultKind(kind)] = count
	}
	return counts, rows.Err()
}

func (s *Store) Finish(ctx context.Context, settlement Settlement, state State, next time.Time, errorClass string) error {
	if settlement.ClaimID == nil || state == StateSettled || state == StateUncertain {
		return ErrClaimLost
	}
	if len(errorClass) > 120 {
		errorClass = errorClass[:120]
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE olp.batch_settlements
		SET state=$3,next_attempt_at=$4,last_error_class=NULLIF($5,''),claim_id=NULL,
		    claimed_until=NULL,updated_at=now()
		WHERE resource_id=$1 AND claim_id=$2
		  AND ($6::text NOT IN ('submitting','submission_unknown') OR state=$6)`,
		settlement.ResourceID, *settlement.ClaimID, state, next, errorClass, settlement.State)
	if err != nil {
		return fmt.Errorf("finish batch settlement: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}

func (s *Store) Close(ctx context.Context, settlement Settlement, state State, gapCount int64, gapDigest []byte, now time.Time) ([]limits.CostSnapshot, error) {
	return s.CloseWithErrorClass(ctx, settlement, state, gapCount, gapDigest, now, "")
}

func (s *Store) CloseWithErrorClass(ctx context.Context, settlement Settlement, state State, gapCount int64, gapDigest []byte, now time.Time, errorClass string) ([]limits.CostSnapshot, error) {
	if settlement.ClaimID == nil || state != StateSettled && state != StateUncertain {
		return nil, ErrClaimLost
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin batch settlement close: %w", err)
	}
	defer tx.Rollback(ctx)
	var snapshots []limits.CostSnapshot
	var uncertainDelta int64
	if gapCount > 0 {
		if len(gapDigest) == 0 {
			return nil, errors.New("batch gap digest is empty")
		}
		result := Result{
			CustomID:   "*",
			Kind:       ResultGap,
			Usage:      usage.AttemptUsage{Observed: false, BillingUncertain: true},
			ErrorClass: "billing_uncertain",
			Evidence:   []byte(fmt.Sprintf("batch-gap:%d", gapCount)),
		}
		evidenceDigest := EvidenceDigest(result.Evidence)
		var existing []byte
		err = tx.QueryRow(ctx, `SELECT evidence_digest FROM olp.batch_settlement_results
			WHERE resource_id=$1 AND custom_id_digest=$2 AND result_kind='gap' FOR UPDATE`,
			settlement.ResourceID, gapDigest).Scan(&existing)
		if err == nil {
			if !equalBytes(existing, evidenceDigest) {
				return nil, errors.New("batch gap evidence conflicts")
			}
		} else if errors.Is(err, pgx.ErrNoRows) {
			event, buildErr := ResultEvent(settlement.Template, settlement.ResourceID, gapDigest, result, now)
			if buildErr != nil {
				return nil, fmt.Errorf("build batch gap event: %w", buildErr)
			}
			eventID, parseErr := uuid.Parse(event.EventID)
			if parseErr != nil {
				return nil, parseErr
			}
			if _, err = tx.Exec(ctx, `INSERT INTO olp.batch_settlement_results
				(resource_id,custom_id_digest,result_kind,event_id,evidence_digest)
				VALUES($1,$2,'gap',$3,$4)`, settlement.ResourceID, gapDigest, eventID, evidenceDigest); err != nil {
				return nil, fmt.Errorf("insert batch gap result: %w", err)
			}
			payload, encodeErr := usage.Encode(event)
			if encodeErr != nil {
				return nil, encodeErr
			}
			persisted, persistErr := usage.PersistEventTx(ctx, tx, event, payload)
			if persistErr != nil {
				return nil, fmt.Errorf("persist batch gap event: %w", persistErr)
			}
			snapshots = append(snapshots, persisted.CostSnapshots...)
			uncertainDelta = gapCount
		} else {
			return nil, fmt.Errorf("read batch gap result: %w", err)
		}
	}
	if state == StateUncertain {
		var resourceExists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.provider_resources WHERE id=$1)`,
			settlement.ResourceID).Scan(&resourceExists); err != nil {
			return nil, fmt.Errorf("check uncertain batch resource: %w", err)
		}
		if !resourceExists {
			if _, err = tx.Exec(ctx, `DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2`,
				settlement.ResourceID, secrets.ProviderContinuation); err != nil {
				return nil, fmt.Errorf("delete uncertain batch contract: %w", err)
			}
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE olp.batch_settlements
		SET state=$3,closed_at=$4,next_attempt_at=$4,uncertain_results=uncertain_results+$5,
		    claim_id=NULL,claimed_until=NULL,last_error_class=NULLIF($6,''),updated_at=$4
		WHERE resource_id=$1 AND claim_id=$2`, settlement.ResourceID, *settlement.ClaimID,
		state, now, uncertainDelta, errorClass)
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

func (s *Store) PurgeClosedBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var deleted int64
	for {
		tag, err := s.Pool.Exec(ctx, `DELETE FROM olp.batch_settlements
			WHERE resource_id IN (
				SELECT resource_id FROM olp.batch_settlements
				WHERE closed_at IS NOT NULL AND closed_at<$1
				ORDER BY closed_at,resource_id LIMIT $2
			)`, cutoff, purgeLimit)
		if err != nil {
			return deleted, fmt.Errorf("purge closed batch settlements: %w", err)
		}
		deleted += tag.RowsAffected()
		if tag.RowsAffected() < purgeLimit {
			return deleted, nil
		}
	}
}

func EvidenceDigest(evidence []byte) []byte {
	sum := sha256.Sum256(evidence)
	return sum[:]
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
