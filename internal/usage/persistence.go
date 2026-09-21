package usage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/limits"
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
	CostSnapshots []limits.CostSnapshot
}

// chargeStatus classifies what an attempt may be charged for.
const (
	chargeNotBillable      = "not_billable"
	chargeBillable         = "billable"
	chargeBillingUncertain = "billing_uncertain"
)

const admitReceiptSQL = `INSERT INTO olp_go.request_metadata_event_receipts
        (event_id, request_id, event_sha256, status, observed_at)
    SELECT $1::uuid, $2::uuid, $3, 'pending', $4
    WHERE $4 >= now() - make_interval(days => $5)
      AND $4 <= now() + make_interval(mins => $6)
      AND NOT EXISTS (SELECT 1 FROM olp_go.attempt_usage_facts
                      WHERE event_id = $1::uuid OR request_id = $2::uuid)
    ON CONFLICT DO NOTHING RETURNING event_id::text`

const existingReceiptSQL = `SELECT
        EXISTS (SELECT 1 FROM olp_go.request_metadata_event_receipts
                WHERE event_id = $1::uuid AND request_id = $2::uuid) AS receipt_exists,
        (SELECT event_sha256 FROM olp_go.request_metadata_event_receipts
         WHERE event_id = $1::uuid AND request_id = $2::uuid) AS event_sha256,
        EXISTS (SELECT 1 FROM olp_go.attempt_usage_facts
                WHERE event_id = $1::uuid AND request_id = $2::uuid) AS attempt_fact_exists,
        ($3 < now() - make_interval(days => $4)
         OR $3 > now() + make_interval(mins => $5)) AS outside_window`

const rejectReceiptSQL = `INSERT INTO olp_go.request_metadata_event_receipts
        (event_id, request_id, event_sha256, status, observed_at)
    SELECT $1::uuid, $2::uuid, $3, 'rejected', $4
    WHERE NOT EXISTS (SELECT 1 FROM olp_go.attempt_usage_facts
                      WHERE event_id = $1::uuid OR request_id = $2::uuid)
    ON CONFLICT DO NOTHING RETURNING event_id::text`

const rejectedGapSQL = `INSERT INTO olp_go.request_metadata_ingestion_gaps
        (id, gateway_instance, event_count, reason, certainty, first_observed_at, last_observed_at)
    VALUES ($1, 'request-metadata-consumer', 0,
            'request_metadata_event_outside_replay_window', 'lower_bound',
            LEAST($2::timestamptz, now()), LEAST($2::timestamptz, now()))`

const raceReceiptSQL = `SELECT EXISTS (
        SELECT 1 FROM olp_go.request_metadata_event_receipts
        WHERE event_id = $1::uuid AND request_id = $2::uuid
          AND event_sha256 = $3
        UNION ALL
        SELECT 1 FROM olp_go.attempt_usage_facts
        WHERE event_id = $1::uuid AND request_id = $2::uuid
          AND NOT EXISTS (SELECT 1 FROM olp_go.request_metadata_event_receipts
                          WHERE event_id = $1::uuid OR request_id = $2::uuid))`

const markReceiptPersistedSQL = `UPDATE olp_go.request_metadata_event_receipts
       SET status = 'fact_persisted'
     WHERE event_id = $1::uuid AND request_id = $2::uuid AND status = 'pending'`

const insertRequestSQL = `INSERT INTO olp_go.requests
        (id, runtime_generation_id, api_key_id, budget_group_id, route_slug, operation, surface,
         started_at, completed_at, status_code, error_class, total_latency_ms, first_byte_ms,
         attempt_count, created_at, attribution, policy_decisions)
    VALUES ($1::uuid, $2::uuid, $3::uuid, $14::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $8, $15::jsonb, $16::jsonb)
    ON CONFLICT (id, started_at) DO NOTHING`

const insertAttemptSQL = `INSERT INTO olp_go.attempts
        (id, request_id, request_started_at, ordinal, provider_id, upstream_model,
         started_at, completed_at, status_code, error_class, committed, latency_ms,
         first_byte_ms, routing)
    VALUES ($1::uuid, $2::uuid, $3, $4, $5::uuid, $6, $7, $8, $9, $10, $11, $12, $13, $14)
    ON CONFLICT (request_id, ordinal) DO NOTHING`

const insertAnchorSQL = `INSERT INTO olp_go.usage_request_anchors (request_id, request_started_at)
    VALUES ($1::uuid, $2) ON CONFLICT DO NOTHING`

// The counted markers are written false and recomputed once every fact of the
// request is in place, so a partially delivered request never counts itself
// twice under one scope.
const insertFactSQL = `INSERT INTO olp_go.attempt_usage_facts
        (attempt_id, event_id, request_id, request_started_at, attempt_ordinal,
         api_key_id, budget_group_id, provider_id, route_slug, upstream_model, operation, surface,
         observed_at, charge_status, usage_observed, usage_complete, input_tokens,
         output_tokens, cached_input_tokens, cache_write_input_tokens,
         cache_write_5m_input_tokens, cache_write_1h_input_tokens,
         media_units, estimated_cost, unpriced,
         pricing_revision_id, currency, request_counted, provider_request_counted,
         model_request_counted, target_request_counted, request_unpriced_counted,
         provider_unpriced_counted, model_unpriced_counted, target_unpriced_counted,
         request_incomplete_counted, provider_incomplete_counted,
         model_incomplete_counted, target_incomplete_counted, attribution)
    VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6::uuid, $24::uuid, $7::uuid, $8, $9, $10, $11, $12,
            $13, $14, $15, $16, $17, $18, $25, $26, $27, $19::numeric, $20::numeric, $21, $22::uuid, $23,
            false, false, false, false, false, false, false, false, false, false, false, false, $28::jsonb)
    ON CONFLICT (request_id, attempt_ordinal) DO NOTHING`

// factTotalsSQL sums exactly the facts this event inserted. The receipt
// admission guarantees no other event's facts share this event identifier, and
// numeric keeps the sum exact.
const factTotalsSQL = `SELECT count(*), COALESCE(sum(estimated_cost), 0)::text,
        count(*) FILTER (WHERE charge_status <> 'not_billable' AND unpriced)
    FROM olp_go.attempt_usage_facts WHERE event_id = $1::uuid`

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
          FROM olp_go.attempt_usage_facts WHERE request_id = $1::uuid)
    UPDATE olp_go.attempt_usage_facts fact SET
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
func PersistEvent(ctx context.Context, pool *pgxpool.Pool, ev *Event, payload []byte) (Persisted, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Persisted{}, fmt.Errorf("persist request metadata event: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	result, err := PersistEventTx(ctx, tx, ev, payload)
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
func PersistEventTx(ctx context.Context, tx pgx.Tx, ev *Event, payload []byte) (Persisted, error) {
	validated, err := Validate(ev)
	if err != nil {
		return Persisted{}, err
	}
	digest := sha256.Sum256(payload)

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
		if err = markReceiptPersisted(ctx, tx, ev); err != nil {
			return Persisted{}, err
		}
		return Persisted{Outcome: PersistOutcomePersisted}, nil
	}

	if _, err = tx.Exec(ctx, insertAnchorSQL, ev.RequestID, ev.RequestStartedAt); err != nil {
		return Persisted{}, fmt.Errorf("persist request metadata anchor: %w", err)
	}
	for _, attempt := range validated.Attempts {
		if err = insertFact(ctx, tx, ev, attempt); err != nil {
			return Persisted{}, err
		}
	}
	snapshots, err := applyCostDelta(ctx, tx, ev)
	if err != nil {
		return Persisted{}, err
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
func admitReceipt(ctx context.Context, tx pgx.Tx, ev *Event, digest []byte) (receiptAdmission, error) {
	var claimed string
	err := tx.QueryRow(ctx, admitReceiptSQL, ev.EventID, ev.RequestID, digest, ev.ObservedAt,
		ReplayHorizonDays, FutureSkewMinutes).Scan(&claimed)
	if err == nil {
		return receiptAcquired, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("admit request metadata receipt: %w", err)
	}

	var receiptExists, factExists, outsideWindow bool
	var storedDigest []byte
	if err = tx.QueryRow(ctx, existingReceiptSQL, ev.EventID, ev.RequestID, ev.ObservedAt,
		ReplayHorizonDays, FutureSkewMinutes).
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
		return 0, fmt.Errorf("%w: event conflicts with an accounted request", ErrInvalidEvent)
	}
	return rejectExpiredReceipt(ctx, tx, ev, digest)
}

// rejectExpiredReceipt records that a delivery arrived after the window in
// which it could still be accounted for, and the gap that admits the totals are
// missing it.
func rejectExpiredReceipt(ctx context.Context, tx pgx.Tx, ev *Event, digest []byte) (receiptAdmission, error) {
	var rejected string
	err := tx.QueryRow(ctx, rejectReceiptSQL, ev.EventID, ev.RequestID, digest, ev.ObservedAt).
		Scan(&rejected)
	switch {
	case err == nil:
		if _, err = tx.Exec(ctx, rejectedGapSQL, uuid7(), ev.ObservedAt); err != nil {
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
	return 0, fmt.Errorf("%w: expired event conflicts with an accounted request", ErrInvalidEvent)
}

func markReceiptPersisted(ctx context.Context, tx pgx.Tx, ev *Event) error {
	if _, err := tx.Exec(ctx, markReceiptPersistedSQL, ev.EventID, ev.RequestID); err != nil {
		return fmt.Errorf("mark request metadata receipt persisted: %w", err)
	}
	return nil
}

func insertRequestRows(ctx context.Context, tx pgx.Tx, ev *Event, validated *Validated) error {
	if _, err := tx.Exec(ctx, insertRequestSQL, ev.RequestID, ev.RuntimeGenerationID, ev.APIKeyID,
		ev.RouteSlug, ev.Operation, ev.Surface, ev.RequestStartedAt, ev.RequestCompletedAt,
		validated.StatusCode, ev.ErrorClass, validated.LatencyMS, validated.FirstByteMS,
		validated.AttemptCount, ev.BudgetGroupID, string(AttributionJSON(ev.Attribution)),
		string(contentpolicy.DecisionsJSON(ev.PolicyDecisions))); err != nil {
		return fmt.Errorf("persist request metadata request: %w", err)
	}
	for _, attempt := range validated.Attempts {
		var routing any
		if attempt.Attempt.Routing != nil {
			routing = attempt.Attempt.Routing
		}
		if _, err := tx.Exec(ctx, insertAttemptSQL, attempt.Attempt.ID, ev.RequestID,
			ev.RequestStartedAt, attempt.Ordinal, attempt.Attempt.ProviderID,
			attempt.Attempt.UpstreamModel, attempt.Attempt.StartedAt, attempt.Attempt.CompletedAt,
			attempt.StatusCode, attempt.Attempt.ErrorClass, attempt.Attempt.Committed,
			attempt.LatencyMS, attempt.FirstByteMS, routing); err != nil {
			return fmt.Errorf("persist request metadata attempt: %w", err)
		}
	}
	return nil
}

// insertFact classifies one attempt and records what it may be charged for. An
// attempt that never reached a provider is not billable and carries no price; an
// attempt whose evidence never arrived is billing uncertain, priced if it can
// be, and never counted as complete.
func insertFact(ctx context.Context, tx pgx.Tx, ev *Event, attempt ValidatedAttempt) error {
	usage := attempt.Usage
	status := chargeNotBillable
	switch {
	case usage.BillingUncertain:
		status = chargeBillingUncertain
	case usage.Observed:
		status = chargeBillable
	}
	pricing := attemptPricing{complete: true}
	if status != chargeNotBillable {
		var err error
		if pricing, err = priceAttempt(ctx, tx, ev, attempt); err != nil {
			return err
		}
	}
	usageComplete := status == chargeNotBillable || usage.Complete
	// A provider that answered successfully but reported no usage has been paid
	// for something this installation cannot quantify; it counts as unpriced
	// rather than as free.
	successfulWithoutUsage := !usage.Observed && attempt.Attempt.ErrorClass == nil &&
		attempt.StatusCode != nil && *attempt.StatusCode >= 200 && *attempt.StatusCode <= 299
	unpriced := status != chargeNotBillable && (successfulWithoutUsage || !pricing.complete)
	if _, err := tx.Exec(ctx, insertFactSQL,
		attempt.Attempt.ID, ev.EventID, ev.RequestID, ev.RequestStartedAt, attempt.Ordinal,
		ev.APIKeyID, attempt.Attempt.ProviderID, ev.RouteSlug, attempt.Attempt.UpstreamModel,
		ev.Operation, ev.Surface, ev.ObservedAt, status, usage.Observed, usageComplete,
		usage.InputTokens, usage.OutputTokens, usage.CachedInputTokens, usage.MediaUnits,
		pricing.estimatedCost, unpriced, pricing.pricingRevisionID, pricing.currency,
		ev.BudgetGroupID, usage.CacheWriteInputTokens, usage.CacheWrite5MInputTokens,
		usage.CacheWrite1HInputTokens, string(AttributionJSON(ev.Attribution)),
	); err != nil {
		return fmt.Errorf("persist attempt usage fact: %w", err)
	}
	return nil
}

// applyCostDelta folds what this event cost into the key's spend windows and
// returns the reconstructed snapshot for the distributed counters. Unpriced
// billable attempts accrue no money but are counted, so an operator can see
// that a budget is being consumed by spend nobody can price.
func applyCostDelta(ctx context.Context, tx pgx.Tx, ev *Event) ([]limits.CostSnapshot, error) {
	var facts, unpriced int64
	var cost string
	if err := tx.QueryRow(ctx, factTotalsSQL, ev.EventID).Scan(&facts, &cost, &unpriced); err != nil {
		return nil, fmt.Errorf("total attempt usage facts: %w", err)
	}
	if facts == 0 {
		return nil, nil
	}
	snapshot, err := limits.AddCostDelta(ctx, tx, ev.APIKeyID, ev.ObservedAt, cost, unpriced)
	if err != nil {
		return nil, fmt.Errorf("apply request metadata cost delta: %w", err)
	}
	snapshots := []limits.CostSnapshot{snapshot}
	if ev.BudgetGroupID != nil {
		group, err := limits.AddGroupCostDelta(ctx, tx, *ev.BudgetGroupID, ev.ObservedAt, cost, unpriced)
		if err != nil {
			return nil, fmt.Errorf("apply request metadata group cost delta: %w", err)
		}
		snapshots = append(snapshots, group)
	}
	return snapshots, nil
}
