package batches

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

var (
	ErrSubmissionInFlight  = errors.New("batch submission outcome unknown")
	ErrClaimLost           = errors.New("batch settlement claim lost")
	ErrResultConflict      = errors.New("batch result evidence conflicts")
	ErrSubmissionAmbiguous = errors.New("batch submission matches competing work")
)

const (
	claimLease = 2 * time.Minute
	purgeLimit = 50000
)

var (
	submissionStates = []State{StateSubmitting, StateSubmissionUnknown}
	claimableStates  = []State{StateSubmitting, StateSubmissionUnknown, StateActive, StateSettling, StateConflict}
	competingStates  = []State{StateSubmitting, StateSubmissionUnknown, StateConflict, StateUncertain}
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

type PageResult struct {
	CustomIDDigest []byte
	Result         Result
}

type Closing struct {
	Decision
	GapDigest  []byte
	ErrorClass ErrorClass
}

func (s *Store) BeginSubmission(ctx context.Context, intent Intent) error {
	if intent.ResourceID == uuid.Nil || intent.Template == nil || intent.Submission == nil || len(intent.SubmissionDigest) == 0 {
		return errors.New("invalid batch submission intent")
	}
	template, err := usage.Encode(intent.Template)
	if err != nil {
		return err
	}
	submission, err := encodeSubmission(intent.Submission)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockSubmissionSourceTx(ctx, tx, intent); err != nil {
		return err
	}
	b := intent.Binding
	_, err = tx.Exec(ctx, `INSERT INTO olp.batch_settlements
		(resource_id,kind,adapter,api_key_id,route_slug,route_revision_id,provider_id,provider_revision_id,
		 target_id,slot_id,credential_id,surface,submission_digest,line_operation,template,state,
		 expected_results,settled_results,uncertain_results,submission,submission_source,creator_deadline,close_after)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,0,0,$18,NULLIF($19,''),$20,$21)`,
		intent.ResourceID, b.Kind, intent.Adapter, b.APIKeyID, b.RouteSlug, b.RouteRevisionID, b.ProviderID,
		b.ProviderRevisionID, b.TargetID, b.SlotID, b.CredentialID, intent.Surface, intent.SubmissionDigest,
		intent.LineOperation, template, StateSubmitting, intent.ExpectedResults, submission,
		intent.Submission.Source(), intent.CreatorDeadline, intent.CloseAfter)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "batch_settlements_submission_identity" {
		return ErrSubmissionInFlight
	}
	if err != nil {
		return fmt.Errorf("insert batch submission intent: %w", err)
	}
	return tx.Commit(ctx)
}

// Intent creation and adoption serialize within the upstream credential/file
// namespace, including callers and route revisions sharing that source.
func lockSubmissionSourceTx(ctx context.Context, tx pgx.Tx, intent Intent) error {
	source := intent.Submission.Source()
	if source == "" {
		return nil
	}
	key, err := json.Marshal([]any{"batch-source", intent.Binding.ProviderID, intent.Binding.SlotID, intent.Binding.CredentialID, source})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, string(key))
	return err
}

func competingSubmissionTx(ctx context.Context, tx pgx.Tx, settlement Settlement, upstreamID string) (bool, error) {
	source := settlement.Submission.Source()
	if source == "" {
		return false, nil
	}
	var competing bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM olp.batch_settlements WHERE provider_id=$1 AND slot_id=$2
		AND credential_id IS NOT DISTINCT FROM $3::uuid AND resource_id<>$4
		AND upstream_id IS NULL AND state=ANY($7) AND submission_source=$5
	) OR EXISTS(
		SELECT 1 FROM olp.batch_settlements WHERE provider_id=$1 AND slot_id=$2
		AND credential_id IS NOT DISTINCT FROM $3::uuid AND resource_id<>$4 AND upstream_id=$6
	)`, settlement.Binding.ProviderID, settlement.Binding.SlotID, settlement.Binding.CredentialID,
		settlement.ResourceID, source, upstreamID, competingStates).Scan(&competing)
	return competing, err
}

func (s *Store) DeleteSubmission(ctx context.Context, resourceID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin batch submission cleanup: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM olp.batch_settlements WHERE resource_id=$1 AND state=ANY($2)`,
		resourceID, submissionStates); err != nil {
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

func (s *Store) Pending(ctx context.Context, resourceID uuid.UUID) (bool, error) {
	var pending bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.batch_settlements WHERE resource_id=$1 AND closed_at IS NULL)`, resourceID).Scan(&pending)
	return pending, err
}

func (s *Store) GeminiResultPending(ctx context.Context, owner *resources.Resource, fileName string) (bool, error) {
	var pending bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.batch_settlements bs
		JOIN olp.provider_resources r ON r.id=bs.resource_id
		WHERE bs.adapter='gemini_managed' AND bs.closed_at IS NULL
		AND bs.api_key_id=$1 AND bs.provider_id=$2 AND bs.slot_id=$3
		AND bs.credential_id IS NOT DISTINCT FROM $4::uuid
		AND ((r.metadata->'provider_batch')#>>'{metadata,output,responsesFile}'=$5
		 OR (r.metadata->'provider_batch')#>>'{metadata,response,responsesFile}'=$5
		 OR (r.metadata->'provider_batch')#>>'{output,responsesFile}'=$5
		 OR (r.metadata->'provider_batch')#>>'{response,responsesFile}'=$5))`,
		owner.APIKeyID, owner.ProviderID, owner.SlotID, owner.CredentialID, fileName).Scan(&pending)
	return pending, err
}

func (s *Store) MarkUnknown(ctx context.Context, resourceID uuid.UUID, errorClass ErrorClass, next time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE olp.batch_settlements
		SET state=$4,last_error_class=$2,next_attempt_at=$3,updated_at=now()
		WHERE resource_id=$1 AND state=$5`, resourceID, errorClass.stored(), next, StateSubmissionUnknown, StateSubmitting)
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
		SET state=$4,upstream_id=$2,expected_results=COALESCE(expected_results,$3),next_attempt_at=now(),
		    last_error_class=NULL,updated_at=now()
		WHERE resource_id=$1 AND state=ANY($5)`, resourceID, upstreamID, expected, StateActive, submissionStates)
	if err != nil {
		return fmt.Errorf("activate batch settlement: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}

func (s *Store) Adopt(ctx context.Context, settlement Settlement, upstreamID string, expected *int64, put func(context.Context, pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockSubmissionSourceTx(ctx, tx, settlement.Intent); err != nil {
		return err
	}
	var claimed bool
	err = tx.QueryRow(ctx, `SELECT true FROM olp.batch_settlements WHERE resource_id=$1 AND claim_id=$2 AND state=$3 FOR UPDATE`,
		settlement.ResourceID, settlement.ClaimID, StateSubmissionUnknown).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrClaimLost
	}
	if err != nil {
		return err
	}
	competing, err := competingSubmissionTx(ctx, tx, settlement, upstreamID)
	if err != nil {
		return err
	}
	if competing {
		return ErrSubmissionAmbiguous
	}
	if err = put(ctx, tx); err != nil {
		return err
	}
	if err = s.ActivateTx(ctx, tx, settlement.ResourceID, upstreamID, expected); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Claim(ctx context.Context, now time.Time, limit int) ([]Settlement, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `WITH due AS (
		SELECT resource_id FROM olp.batch_settlements
		WHERE state=ANY($3) AND next_attempt_at<=$1 AND (claim_id IS NULL OR claimed_until<=$1)
		ORDER BY next_attempt_at,resource_id LIMIT $2 FOR UPDATE SKIP LOCKED)
		UPDATE olp.batch_settlements b
		SET claim_id=gen_random_uuid(),claimed_until=$4,attempts=attempts+1,updated_at=$1
		FROM due WHERE b.resource_id=due.resource_id
		RETURNING `+settlementColumns, now, limit, claimableStates, now.Add(claimLease))
	if err != nil {
		return nil, fmt.Errorf("claim batch settlements: %w", err)
	}
	settlements, err := pgx.CollectRows(rows, scanSettlement)
	if err != nil {
		return nil, fmt.Errorf("scan batch settlement claims: %w", err)
	}
	return settlements, nil
}

const settlementColumns = `b.resource_id,b.kind,b.adapter,b.api_key_id,b.route_slug,b.route_revision_id,b.provider_id,
	b.provider_revision_id,b.target_id,b.slot_id,b.credential_id,COALESCE(b.upstream_id,''),b.surface,
	b.submission_digest,b.line_operation,b.template,b.state,b.expected_results,b.cursor,b.submission,
	b.creator_deadline,b.claim_id,b.attempts,b.close_after,b.created_at`

func scanSettlement(row pgx.CollectableRow) (Settlement, error) {
	var settlement Settlement
	var template, cursor, submission []byte
	b := &settlement.Binding
	err := row.Scan(&settlement.ResourceID, &b.Kind, &settlement.Adapter, &b.APIKeyID, &b.RouteSlug,
		&b.RouteRevisionID, &b.ProviderID, &b.ProviderRevisionID, &b.TargetID, &b.SlotID, &b.CredentialID,
		&settlement.UpstreamID, &settlement.Surface, &settlement.SubmissionDigest, &settlement.LineOperation,
		&template, &settlement.State, &settlement.ExpectedResults, &cursor, &submission,
		&settlement.CreatorDeadline, &settlement.ClaimID, &settlement.Attempts, &settlement.CloseAfter,
		&settlement.CreatedAt)
	if err != nil {
		return Settlement{}, err
	}
	if err = json.Unmarshal(cursor, &settlement.Cursor); err != nil {
		return Settlement{}, fmt.Errorf("decode batch cursor: %w", err)
	}
	if settlement.Submission, err = decodeSubmission(submission); err != nil {
		return Settlement{}, err
	}
	if settlement.Template, err = usage.Decode(template); err != nil {
		return Settlement{}, err
	}
	return settlement, nil
}

func (s *Store) ExtendClaim(ctx context.Context, settlement Settlement, now time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE olp.batch_settlements
		SET claimed_until=$3,updated_at=$4 WHERE resource_id=$1 AND claim_id=$2 AND state=ANY($5)`,
		settlement.ResourceID, settlement.ClaimID, now.Add(claimLease), now, claimableStates)
	if err != nil {
		return fmt.Errorf("extend batch settlement claim: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}

func (s *Store) SetExpected(ctx context.Context, settlement Settlement, expected *int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE olp.batch_settlements SET expected_results=COALESCE(expected_results,$3),updated_at=now()
		WHERE resource_id=$1 AND claim_id=$2`, settlement.ResourceID, settlement.ClaimID, expected)
	if err != nil {
		return fmt.Errorf("update batch expected result count: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}

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
	persisted, err := usage.PersistEventTx(ctx, tx, accepted)
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
		Usage:      usage.AttemptUsage{Observed: false, BillingUncertain: true},
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

func (s *Store) PurgeClosedBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var deleted int64
	for {
		tag, err := s.pool.Exec(ctx, `DELETE FROM olp.batch_settlements
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
