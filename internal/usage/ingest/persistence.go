package ingest

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/ledger"
	"github.com/tyk-swe/olp/internal/usage"
)

// PersistOutcome is what one delivery did to the accounting tables.
type PersistOutcome int

const (
	// PersistOutcomePersisted is a first delivery that became durable facts.
	PersistOutcomePersisted PersistOutcome = iota
	// PersistOutcomeDuplicate is a delivery that was already accounted for.
	// Re-delivery is normal: the stream guarantees at least once.
	PersistOutcomeDuplicate
	// PersistOutcomeRejectedOutsideReplayWindow is a delivery that arrived too
	// late to belong to any aggregate it could still change. It is recorded as
	// a gap instead of being silently added to today's totals.
	PersistOutcomeRejectedOutsideReplayWindow
)

// Persisted is the result of one persistence attempt, with the spend snapshot
// that the caller pushes back into the distributed cost counters.
type Persisted struct {
	Outcome       PersistOutcome
	CostSnapshots []limits.Snapshot
}

// chargeStatus classifies what an attempt may be charged for.
const (
	chargeNotBillable      = "not_billable"
	chargeBillable         = "billable"
	chargeBillingUncertain = "billing_uncertain"
)

const admitReceiptSQL = `INSERT INTO olp.request_metadata_event_receipts
        (event_id, request_id, event_sha256, status, observed_at)
    SELECT $1::uuid, $2::uuid, $3, 'pending', $4
    WHERE $4 >= now() - make_interval(days => $5)
      AND $4 <= now() + make_interval(mins => $6)
      AND NOT EXISTS (SELECT 1 FROM olp.attempt_usage_facts
                      WHERE event_id = $1::uuid OR request_id = $2::uuid)
    ON CONFLICT DO NOTHING RETURNING event_id::text`

const existingReceiptSQL = `SELECT
        EXISTS (SELECT 1 FROM olp.request_metadata_event_receipts
                WHERE event_id = $1::uuid AND request_id = $2::uuid) AS receipt_exists,
        (SELECT event_sha256 FROM olp.request_metadata_event_receipts
         WHERE event_id = $1::uuid AND request_id = $2::uuid) AS event_sha256,
        EXISTS (SELECT 1 FROM olp.attempt_usage_facts
                WHERE event_id = $1::uuid AND request_id = $2::uuid) AS attempt_fact_exists,
        ($3 < now() - make_interval(days => $4)
         OR $3 > now() + make_interval(mins => $5)) AS outside_window`

const rejectReceiptSQL = `INSERT INTO olp.request_metadata_event_receipts
        (event_id, request_id, event_sha256, status, observed_at)
    SELECT $1::uuid, $2::uuid, $3, 'rejected', $4
    WHERE NOT EXISTS (SELECT 1 FROM olp.attempt_usage_facts
                      WHERE event_id = $1::uuid OR request_id = $2::uuid)
    ON CONFLICT DO NOTHING RETURNING event_id::text`

const rejectedGapSQL = `INSERT INTO olp.request_metadata_ingestion_gaps
        (id, gateway_instance, event_count, reason, certainty, first_observed_at, last_observed_at)
    VALUES ($1, 'request-metadata-consumer', 0,
            'request_metadata_event_outside_replay_window', 'lower_bound',
            LEAST($2::timestamptz, now()), LEAST($2::timestamptz, now()))`

const raceReceiptSQL = `SELECT EXISTS (
        SELECT 1 FROM olp.request_metadata_event_receipts
        WHERE event_id = $1::uuid AND request_id = $2::uuid
          AND event_sha256 = $3
        UNION ALL
        SELECT 1 FROM olp.attempt_usage_facts
        WHERE event_id = $1::uuid AND request_id = $2::uuid
          AND NOT EXISTS (SELECT 1 FROM olp.request_metadata_event_receipts
                          WHERE event_id = $1::uuid OR request_id = $2::uuid))`

const markReceiptPersistedSQL = `UPDATE olp.request_metadata_event_receipts
       SET status = 'fact_persisted'
     WHERE event_id = $1::uuid AND request_id = $2::uuid AND status = 'pending'`

const insertRequestSQL = `INSERT INTO olp.requests
        (id, runtime_generation_id, api_key_id, budget_group_id, route_slug, operation, surface,
         started_at, completed_at, status_code, error_class, total_latency_ms, first_byte_ms,
         attempt_count, created_at, attribution, policy_decisions, origin, parent_request_id, end_user_digest, budget_boundary, payload_captured,guardrail_decisions,evaluator_guardrail_id,cache_result)
    VALUES ($1::uuid, $2::uuid, NULLIF($3, '')::uuid, $14::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, COALESCE($8, now()),
            $15::jsonb, $16::jsonb, $17, $18::uuid, $19, $20, $21,$22::jsonb,$23::uuid,NULLIF($24,''))
    ON CONFLICT (id, started_at) DO NOTHING`

const insertAttemptSQL = `INSERT INTO olp.attempts
        (id, request_id, request_started_at, ordinal, provider_id, upstream_model,
         started_at, completed_at, status_code, error_class, committed, latency_ms,
         first_byte_ms, routing,guardrail_decisions)
    VALUES ($1::uuid, $2::uuid, $3, $4, $5::uuid, $6, $7, $8, $9, $10, $11, $12, $13, $14,$15::jsonb)
    ON CONFLICT (request_id, ordinal) DO NOTHING`

const insertAnchorSQL = `INSERT INTO olp.usage_request_anchors (request_id, request_started_at)
    VALUES ($1::uuid, $2) ON CONFLICT DO NOTHING`

// recomputeMarkersSQL decides, for each count scope, which attempt of a request
// carries the request. Reports count the first attempt of a scope once, and
// attribute the scope's unpriced or incomplete state to that same row, so a
// request that failed over three times is still one request.
const recomputeMarkersSQL = `WITH marked AS (
        SELECT attempt_id,
               row_number() OVER (PARTITION BY request_id ORDER BY attempt_ordinal) = 1
                   AS request_marker,
               row_number() OVER (PARTITION BY request_id, provider_id
                                  ORDER BY attempt_ordinal) = 1 AS provider_marker,
               row_number() OVER (PARTITION BY request_id, upstream_model
                                  ORDER BY attempt_ordinal) = 1 AS model_marker,
               row_number() OVER (PARTITION BY request_id, provider_id, upstream_model
                                  ORDER BY attempt_ordinal) = 1 AS target_marker,
               bool_or(charge_status <> 'not_billable' AND unpriced)
                   OVER (PARTITION BY request_id) AS request_unpriced,
               bool_or(charge_status <> 'not_billable' AND unpriced)
                   OVER (PARTITION BY request_id, provider_id) AS provider_unpriced,
               bool_or(charge_status <> 'not_billable' AND unpriced)
                   OVER (PARTITION BY request_id, upstream_model) AS model_unpriced,
               bool_or(charge_status <> 'not_billable' AND unpriced)
                   OVER (PARTITION BY request_id, provider_id, upstream_model) AS target_unpriced,
               bool_or(charge_status <> 'not_billable' AND NOT usage_complete)
                   OVER (PARTITION BY request_id) AS request_incomplete,
               bool_or(charge_status <> 'not_billable' AND NOT usage_complete)
                   OVER (PARTITION BY request_id, provider_id) AS provider_incomplete,
               bool_or(charge_status <> 'not_billable' AND NOT usage_complete)
                   OVER (PARTITION BY request_id, upstream_model) AS model_incomplete,
               bool_or(charge_status <> 'not_billable' AND NOT usage_complete)
                   OVER (PARTITION BY request_id, provider_id, upstream_model) AS target_incomplete
          FROM olp.attempt_usage_facts WHERE request_id = $1::uuid)
    UPDATE olp.attempt_usage_facts fact SET
        request_counted = marked.request_marker,
        provider_request_counted = marked.provider_marker,
        model_request_counted = marked.model_marker,
        target_request_counted = marked.target_marker,
        request_unpriced_counted = marked.request_marker AND marked.request_unpriced,
        provider_unpriced_counted = marked.provider_marker AND marked.provider_unpriced,
        model_unpriced_counted = marked.model_marker AND marked.model_unpriced,
        target_unpriced_counted = marked.target_marker AND marked.target_unpriced,
        request_incomplete_counted = marked.request_marker AND marked.request_incomplete,
        provider_incomplete_counted = marked.provider_marker AND marked.provider_incomplete,
        model_incomplete_counted = marked.model_marker AND marked.model_incomplete,
        target_incomplete_counted = marked.target_marker AND marked.target_incomplete
      FROM marked WHERE fact.attempt_id = marked.attempt_id`

// receiptAdmission is what the idempotency receipt says about this delivery.
type receiptAdmission int

const (
	receiptAcquired receiptAdmission = iota
	receiptDuplicate
	receiptRejected
)

// PersistEvent turns one delivered request metadata event into durable request
// history and usage facts, priced against the revision in force when it was
// observed, and returns the spend snapshot the caller pushes back into the
// distributed budget counters.
//
// The whole event is one transaction: history, facts, spend windows and the
// receipt that makes a replay a duplicate all commit together, so a delivery is
// either fully accounted for or not at all. `payload` is the original stream
// bytes, fingerprinted so a replay is recognised even if this build serializes
// the event differently than the one that wrote it.
func PersistEvent(ctx context.Context, pool *pgxpool.Pool, accepted usage.AccountingEvent) (Persisted, error) {
	if accepted.Validated() == nil {
		return Persisted{}, usage.ErrInvalidEvent
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Persisted{}, fmt.Errorf("persist request metadata event: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	result, err := PersistEventTx(ctx, tx, accepted)
	if err != nil {
		return Persisted{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Persisted{}, fmt.Errorf("persist request metadata event: %w", err)
	}
	return result, nil
}

// PersistEventTx records accounting in the caller's transaction, allowing a
// resource's reconciliation marker and its usage to commit atomically.
func PersistEventTx(ctx context.Context, tx pgx.Tx, accepted usage.AccountingEvent) (Persisted, error) {
	if accepted.Validated() == nil {
		return Persisted{}, usage.ErrInvalidEvent
	}
	ev := accepted.Event()
	if workflowEvent(ev) {
		return persistWorkflowTerminalTx(ctx, tx, accepted)
	}
	if _, present := debitDispatchID(ev); present {
		return persistDebitEventTx(ctx, tx, accepted)
	}
	return persistEventTx(ctx, tx, accepted, nil)
}

func persistEventTx(ctx context.Context, tx pgx.Tx, accepted usage.AccountingEvent, supplyOwners []string) (Persisted, error) {
	ev, validated, digest := accepted.Event(), accepted.Validated(), accepted.Digest()

	admission, err := admitReceipt(ctx, tx, ev, digest[:])
	if err != nil {
		return Persisted{}, err
	}
	switch admission {
	case receiptDuplicate:
		// Nothing was written; the earlier delivery owns this event.
		return Persisted{Outcome: PersistOutcomeDuplicate}, nil
	case receiptRejected:
		// The rejection and its gap are the record of this delivery.
		return Persisted{Outcome: PersistOutcomeRejectedOutsideReplayWindow}, nil
	}

	if err = insertRequestRows(ctx, tx, ev, validated); err != nil {
		return Persisted{}, err
	}
	if !validated.HasAttempts {
		// An authenticated request that failed before any provider attempt is
		// worth remembering, but there is no usage to price or roll up.
		snapshots, err := applyAttemptIdentityCostDelta(ctx, tx, ev, ledger.Delta{ObservedAt: ev.ObservedAt, Cost: "0", Charge: "0"}, nil)
		if err != nil {
			return Persisted{}, err
		}
		if err = markReceiptPersisted(ctx, tx, ev); err != nil {
			return Persisted{}, err
		}
		return Persisted{Outcome: PersistOutcomePersisted, CostSnapshots: snapshots}, nil
	}

	if _, err = tx.Exec(ctx, insertAnchorSQL, ev.RequestID, ev.RequestStartedAt); err != nil {
		return Persisted{}, fmt.Errorf("persist request metadata anchor: %w", err)
	}
	supply := map[string]limits.Snapshot{}
	for _, attempt := range validated.Attempts {
		charge, err := insertFact(ctx, tx, ev, attempt)
		if err != nil {
			return Persisted{}, err
		}
		if err = chargeSupply(ctx, tx, ev, attempt, charge, supply, supplyOwners); err != nil {
			return Persisted{}, err
		}
	}
	snapshots, err := applyCostDelta(ctx, tx, ev)
	if err != nil {
		return Persisted{}, err
	}
	for _, owner := range slices.Sorted(maps.Keys(supply)) {
		snapshots = append(snapshots, supply[owner])
	}
	if _, err = tx.Exec(ctx, recomputeMarkersSQL, ev.RequestID); err != nil {
		return Persisted{}, fmt.Errorf("recompute usage markers: %w", err)
	}
	if err = markReceiptPersisted(ctx, tx, ev); err != nil {
		return Persisted{}, err
	}
	return Persisted{Outcome: PersistOutcomePersisted, CostSnapshots: snapshots}, nil
}

// admitReceipt claims the right to account for this event exactly once, inside
// the window where doing so can still be correct.
func admitReceipt(ctx context.Context, tx pgx.Tx, ev *usage.Event, digest []byte) (receiptAdmission, error) {
	var claimed string
	err := tx.QueryRow(ctx, admitReceiptSQL, ev.EventID, ev.RequestID, digest, ev.ObservedAt,
		usage.ReplayHorizonDays, usage.FutureSkewMinutes).Scan(&claimed)
	if err == nil {
		return receiptAcquired, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("admit request metadata receipt: %w", err)
	}

	var receiptExists, factExists, outsideWindow bool
	var storedDigest []byte
	if err = tx.QueryRow(ctx, existingReceiptSQL, ev.EventID, ev.RequestID, ev.ObservedAt,
		usage.ReplayHorizonDays, usage.FutureSkewMinutes).
		Scan(&receiptExists, &storedDigest, &factExists, &outsideWindow); err != nil {
		return 0, fmt.Errorf("admit request metadata receipt: %w", err)
	}
	// The same event delivered twice is a duplicate; the same identifiers
	// carrying different bytes is not, and must never overwrite what was
	// already accounted for.
	exact := receiptExists && storedDigest != nil && string(storedDigest) == string(digest)
	if exact || (!receiptExists && factExists) {
		return receiptDuplicate, nil
	}
	if receiptExists || !outsideWindow {
		return 0, fmt.Errorf("%w: event conflicts with an accounted request", usage.ErrInvalidEvent)
	}
	return rejectExpiredReceipt(ctx, tx, ev, digest)
}

// rejectExpiredReceipt records that a delivery arrived after the window in
// which it could still be accounted for, and the gap that admits the totals are
// missing it.
func rejectExpiredReceipt(ctx context.Context, tx pgx.Tx, ev *usage.Event, digest []byte) (receiptAdmission, error) {
	var rejected string
	err := tx.QueryRow(ctx, rejectReceiptSQL, ev.EventID, ev.RequestID, digest, ev.ObservedAt).
		Scan(&rejected)
	switch {
	case err == nil:
		if _, err = tx.Exec(ctx, rejectedGapSQL, uuid.Must(uuid.NewV7()).String(), ev.ObservedAt); err != nil {
			return 0, fmt.Errorf("record expired request metadata receipt: %w", err)
		}
		return receiptRejected, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("record expired request metadata receipt: %w", err)
	}
	// Another consumer admitted or accounted for this event between the two
	// statements above. That is a duplicate, not a conflict.
	var settled bool
	if err = tx.QueryRow(ctx, raceReceiptSQL, ev.EventID, ev.RequestID, digest).Scan(&settled); err != nil {
		return 0, fmt.Errorf("record expired request metadata receipt: %w", err)
	}
	if settled {
		return receiptDuplicate, nil
	}
	return 0, fmt.Errorf("%w: expired event conflicts with an accounted request", usage.ErrInvalidEvent)
}

func markReceiptPersisted(ctx context.Context, tx pgx.Tx, ev *usage.Event) error {
	if _, err := tx.Exec(ctx, markReceiptPersistedSQL, ev.EventID, ev.RequestID); err != nil {
		return fmt.Errorf("mark request metadata receipt persisted: %w", err)
	}
	return nil
}

func insertRequestRows(ctx context.Context, tx pgx.Tx, ev *usage.Event, validated *usage.Validated) error {
	var completedAt, latency any = ev.RequestCompletedAt, validated.LatencyMS
	if ev.HTTPOutcomePending {
		completedAt, latency = nil, nil
	}
	if _, err := tx.Exec(ctx, insertRequestSQL, ev.RequestID, ev.RuntimeGenerationID, ev.APIKeyID,
		ev.RouteSlug, ev.Operation, ev.Surface, ev.RequestStartedAt, completedAt,
		validated.StatusCode, ev.ErrorClass, latency, validated.FirstByteMS,
		validated.AttemptCount, ev.BudgetGroupID, string(attribution.JSON(ev.Attribution)),
		string(contentpolicy.DecisionsJSON(ev.PolicyDecisions)), cmp.Or(ev.Origin, usage.OriginCaller),
		ev.ParentRequestID, ev.EndUserDigest, ev.BudgetBoundary, ev.PayloadCaptured, guardrailJSON(ev.GuardrailDecisions), ev.EvaluatorGuardrailID, ev.CacheResult); err != nil {
		return fmt.Errorf("persist request metadata request: %w", err)
	}
	for _, attempt := range validated.Attempts {
		var attemptCompletedAt, attemptLatency any = attempt.Attempt.CompletedAt, attempt.LatencyMS
		if ev.HTTPOutcomePending && attempt.Ordinal == validated.AttemptCount {
			attemptCompletedAt, attemptLatency = nil, nil
		}
		var routing any
		if attempt.Attempt.Routing != nil {
			routing = attempt.Attempt.Routing
		}
		if _, err := tx.Exec(ctx, insertAttemptSQL, attempt.Attempt.ID, ev.RequestID,
			ev.RequestStartedAt, attempt.Ordinal, attempt.Attempt.ProviderID,
			attempt.Attempt.UpstreamModel, attempt.Attempt.StartedAt, attemptCompletedAt,
			attempt.StatusCode, attempt.Attempt.ErrorClass, attempt.Attempt.Committed,
			attemptLatency, attempt.FirstByteMS, routing, guardrailJSON(attempt.Attempt.GuardrailDecisions)); err != nil {
			return fmt.Errorf("persist request metadata attempt: %w", err)
		}
	}
	return nil
}

func named(snapshots []limits.Snapshot, requestID string) []limits.Snapshot {
	out := make([]limits.Snapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		snapshot.RequestID = requestID
		out = append(out, snapshot)
	}
	return out
}

func guardrailJSON(v any) string {
	b, _ := json.Marshal(v)
	if string(b) == "null" {
		return "[]"
	}
	return string(b)
}
