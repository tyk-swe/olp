package usage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/limits"
)

// WorkflowEventID gives each request in a workflow its own stable receipt identity.
func WorkflowEventID(runID, requestID string) (uuid.UUID, error) {
	run, err := uuid.Parse(runID)
	if err != nil {
		return uuid.Nil, err
	}
	request, err := uuid.Parse(requestID)
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.NewSHA1(run, []byte("request:"+request.String())), nil
}

func workflowReferenceMatches(event *Event, ref *ExecutionReference, attemptID string) bool {
	if ref == nil || ref.RunID == "" || ref.DispatchID != attemptID {
		return false
	}
	if ref.RunID == event.EventID {
		return true
	}
	id, err := WorkflowEventID(ref.RunID, event.RequestID)
	return err == nil && id.String() == event.EventID
}

func workflowRunID(event *Event) string {
	if event == nil {
		return ""
	}
	for _, attempt := range event.Attempts {
		if attempt.Execution != nil && attempt.Execution.RunID != "" {
			return attempt.Execution.RunID
		}
	}
	return ""
}

func workflowEvent(event *Event) bool {
	return workflowRunID(event) != ""
}

func persistWorkflowTerminalTx(ctx context.Context, tx pgx.Tx, event *Event, payload []byte) (Persisted, error) {
	validated, err := Validate(event)
	if err != nil {
		return Persisted{}, err
	}
	runID := workflowRunID(event)
	for _, attempt := range validated.Attempts {
		ref := attempt.Attempt.Execution
		if ref == nil {
			continue
		}
		if !workflowReferenceMatches(event, ref, attempt.Attempt.ID) || runID != ref.RunID {
			return Persisted{}, limits.ErrDebitConflict
		}
	}
	digest := sha256.Sum256(payload)
	var claimed string
	claimErr := tx.QueryRow(ctx, `INSERT INTO olp.request_metadata_event_receipts(event_id,request_id,event_sha256,status,observed_at)
SELECT $1,$2,$3,'pending',$4 WHERE $4 >= now()-make_interval(days=>$5) AND $4 <= now()+make_interval(mins=>$6)
AND NOT EXISTS(SELECT 1 FROM olp.attempt_usage_facts WHERE (event_id=$1 OR request_id=$2) AND NOT (event_id=$1 AND request_id=$2))
ON CONFLICT DO NOTHING RETURNING event_id::text`, event.EventID, event.RequestID, digest[:], event.ObservedAt, ReplayHorizonDays, FutureSkewMinutes).Scan(&claimed)
	if errors.Is(claimErr, pgx.ErrNoRows) {
		var existing []byte
		if err := tx.QueryRow(ctx, `SELECT event_sha256 FROM olp.request_metadata_event_receipts WHERE event_id=$1 AND request_id=$2`, event.EventID, event.RequestID).Scan(&existing); err == nil && string(existing) == string(digest[:]) {
			return Persisted{Outcome: PersistOutcomeDuplicate}, nil
		}
		return Persisted{}, fmt.Errorf("%w: workflow receipt conflicts with accounting", ErrInvalidEvent)
	}
	if claimErr != nil {
		return Persisted{}, claimErr
	}
	if err := insertRequestRows(ctx, tx, event, validated); err != nil {
		return Persisted{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.requests SET completed_at=$3,status_code=$4,error_class=$5,total_latency_ms=$6,first_byte_ms=$7,attempt_count=$8,policy_decisions=$9::jsonb,budget_boundary=$10,payload_captured=$11,guardrail_decisions=$12::jsonb,evaluator_guardrail_id=$13::uuid,cache_result=NULLIF($14,'') WHERE id=$1 AND started_at=$2`, event.RequestID, event.RequestStartedAt, event.RequestCompletedAt, validated.StatusCode, event.ErrorClass, validated.LatencyMS, validated.FirstByteMS, validated.AttemptCount, string(contentpolicy.DecisionsJSON(event.PolicyDecisions)), event.BudgetBoundary, event.PayloadCaptured, guardrailJSON(event.GuardrailDecisions), event.EvaluatorGuardrailID, event.CacheResult); err != nil {
		return Persisted{}, err
	}
	if _, err := tx.Exec(ctx, insertAnchorSQL, event.RequestID, event.RequestStartedAt); err != nil {
		return Persisted{}, err
	}
	for _, attempt := range validated.Attempts {
		if _, err := tx.Exec(ctx, `UPDATE olp.attempts SET completed_at=$3,status_code=$4,error_class=$5,committed=$6,latency_ms=$7,first_byte_ms=$8 WHERE id=$1 AND request_id=$2`, attempt.Attempt.ID, event.RequestID, attempt.Attempt.CompletedAt, attempt.StatusCode, attempt.Attempt.ErrorClass, attempt.Attempt.Committed, attempt.LatencyMS, attempt.FirstByteMS); err != nil {
			return Persisted{}, err
		}
		ref := attempt.Attempt.Execution
		if ref != nil {
			var existing bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.execution_debits WHERE id=$1 AND run_id=$2 AND step_id=$3 AND api_key_id=$4 AND kind='model' AND state IN ('dispatched','outcome_unknown','settled'))`, ref.DispatchID, ref.RunID, ref.StepID, event.APIKeyID).Scan(&existing); err != nil {
				return Persisted{}, err
			}
			if !existing {
				return Persisted{}, limits.ErrDebitConflict
			}
			continue
		}
		if attempt.Usage.Observed || attempt.Usage.BillingUncertain {
			return Persisted{}, limits.ErrDebitConflict
		}
		if _, err := insertFact(ctx, tx, event, attempt); err != nil {
			return Persisted{}, err
		}
	}
	if _, err := tx.Exec(ctx, recomputeMarkersSQL, event.RequestID); err != nil {
		return Persisted{}, err
	}
	if err := markReceiptPersisted(ctx, tx, event); err != nil {
		return Persisted{}, err
	}
	return Persisted{Outcome: PersistOutcomePersisted}, nil
}

func PersistWorkflowAttempt(ctx context.Context, pool *pgxpool.Pool, event *Event, index int) ([]limits.CostSnapshot, error) {
	validated, err := Validate(event)
	if err != nil {
		return nil, err
	}
	if index < 0 || index != len(validated.Attempts)-1 {
		return nil, limits.ErrDebitConflict
	}
	attempt := validated.Attempts[index]
	ref := attempt.Attempt.Execution
	if !workflowReferenceMatches(event, ref, attempt.Attempt.ID) {
		return nil, limits.ErrDebitConflict
	}
	payload, err := json.Marshal(attempt.Attempt)
	if err != nil {
		return nil, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	single := &Event{APIKeyID: event.APIKeyID, RuntimeGenerationID: event.RuntimeGenerationID, Attempts: []Attempt{*attempt.Attempt}}
	currency, owners, err := validateDebitEvent(ctx, tx, single, ref.DispatchID)
	if err != nil {
		return nil, err
	}
	identity := sessionDebitResult(ref.DispatchID, event, payload, "0", "0", false)
	var retainedCost, retainedCharge string
	var retainedComplete bool
	replayErr := tx.QueryRow(ctx, `SELECT cost::text,charge::text,complete FROM olp.execution_debit_results WHERE dispatch_id=$1 AND result_digest=$2`, ref.DispatchID, identity.ResultDigest).Scan(&retainedCost, &retainedCharge, &retainedComplete)
	if replayErr != nil && !errors.Is(replayErr, pgx.ErrNoRows) {
		return nil, replayErr
	}
	if replayErr == nil {
		retained := sessionDebitResult(ref.DispatchID, event, payload, retainedCost, retainedCharge, retainedComplete)
		if _, err := limits.DebitResultReplayTx(ctx, tx, retained); err != nil {
			return nil, err
		}
		if !retainedComplete {
			return nil, fmt.Errorf("workflow model usage is incomplete")
		}
		return nil, nil
	}
	if err := insertRequestRows(ctx, tx, event, validated); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.requests SET completed_at=NULL,status_code=NULL,error_class=NULL,total_latency_ms=NULL,first_byte_ms=NULL,attempt_count=$3 WHERE id=$1 AND started_at=$2`, event.RequestID, event.RequestStartedAt, validated.AttemptCount); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, insertAnchorSQL, event.RequestID, event.RequestStartedAt); err != nil {
		return nil, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.attempt_usage_facts WHERE attempt_id=$1 OR (request_id=$2 AND attempt_ordinal=$3))`, attempt.Attempt.ID, event.RequestID, attempt.Ordinal).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, limits.ErrDebitConflict
	}
	charge, err := insertFact(ctx, tx, event, attempt)
	if err != nil {
		return nil, err
	}
	supply := map[string]limits.CostSnapshot{}
	if err := chargeSupply(ctx, tx, event, attempt, charge, supply, owners); err != nil {
		return nil, err
	}
	snapshots, err := applyAttemptCostDelta(ctx, tx, event, &attempt.Attempt.ID)
	if err != nil {
		return nil, err
	}
	supplySnapshots := slices.Collect(func(yield func(limits.CostSnapshot) bool) {
		for _, owner := range slices.Sorted(maps.Keys(supply)) {
			if !yield(supply[owner]) {
				return
			}
		}
	})
	allSnapshots := slices.Concat(snapshots, supplySnapshots)
	if _, err := tx.Exec(ctx, recomputeMarkersSQL, event.RequestID); err != nil {
		return nil, err
	}
	var cost, billed string
	var factsComplete bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(estimated_cost,0)::text,COALESCE(charge,0)::text,charge_status='not_billable' OR (usage_complete AND NOT unpriced AND estimated_cost IS NOT NULL AND charge IS NOT NULL AND (currency=$2 OR (estimated_cost=0 AND charge=0))) FROM olp.attempt_usage_facts WHERE attempt_id=$1`, attempt.Attempt.ID, currency).Scan(&cost, &billed, &factsComplete); err != nil {
		return nil, err
	}
	complete := factsComplete && attempt.Attempt.Usage != nil && attempt.Attempt.Usage.Complete && !attempt.Attempt.Usage.BillingUncertain
	result := sessionDebitResult(ref.DispatchID, event, payload, cost, billed, complete)
	if err := limits.SettleDebitTx(ctx, tx, result, allSnapshots); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if !complete {
		return allSnapshots, fmt.Errorf("workflow model usage is incomplete")
	}
	return allSnapshots, nil
}
