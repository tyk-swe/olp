package usage

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"

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
         attempt_count, created_at, attribution, policy_decisions, origin, parent_request_id, end_user_digest, budget_boundary, payload_captured)
    VALUES ($1::uuid, $2::uuid, NULLIF($3, '')::uuid, $14::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $8,
            $15::jsonb, $16::jsonb, $17, $18::uuid, $19, $20, $21)
    ON CONFLICT (id, started_at) DO NOTHING`

const insertAttemptSQL = `INSERT INTO olp.attempts
        (id, request_id, request_started_at, ordinal, provider_id, upstream_model,
         started_at, completed_at, status_code, error_class, committed, latency_ms,
         first_byte_ms, routing)
    VALUES ($1::uuid, $2::uuid, $3, $4, $5::uuid, $6, $7, $8, $9, $10, $11, $12, $13, $14)
    ON CONFLICT (request_id, ordinal) DO NOTHING`

const insertAnchorSQL = `INSERT INTO olp.usage_request_anchors (request_id, request_started_at)
    VALUES ($1::uuid, $2) ON CONFLICT DO NOTHING`

// The counted markers are written false and recomputed once every fact of the
// request is in place, so a partially delivered request never counts itself
// twice under one scope.
const insertFactSQL = `INSERT INTO olp.attempt_usage_facts
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
         model_incomplete_counted, target_incomplete_counted, attribution,
         estimated_input_tokens, estimate_provenance, model_family, selector, baseline_cost, end_user_digest, budget_exempt)
    VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, NULLIF($6, '')::uuid, $24::uuid, $7::uuid, $8, $9, $10, $11, $12,
            $13, $14, $15, $16, $17, $18, $25, $26, $27, $19::numeric, $20::numeric, $21, $22::uuid, $23,
            false, false, false, false, false, false, false, false, false, false, false, false, $28::jsonb,
            $29, $30, $31, $32, $33::numeric, $34, $35)
    ON CONFLICT (request_id, attempt_ordinal) DO NOTHING`

// factTotalsSQL sums exactly the facts this event inserted. The receipt
// admission guarantees no other event's facts share this event identifier, and
// numeric keeps the sum exact.
const factTotalsSQL = `SELECT count(*), COALESCE(sum(estimated_cost) FILTER (WHERE NOT budget_exempt), 0)::text,
        count(*) FILTER (WHERE NOT budget_exempt AND charge_status <> 'not_billable' AND unpriced)
    FROM olp.attempt_usage_facts WHERE event_id = $1::uuid`

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
		snapshots, err := applyIdentityCostDelta(ctx, tx, ev, "0", 0)
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
	supply := map[string]limits.CostSnapshot{}
	for _, attempt := range validated.Attempts {
		charge, err := insertFact(ctx, tx, ev, attempt)
		if err != nil {
			return Persisted{}, err
		}
		if err = chargeSupply(ctx, tx, ev, attempt, charge, supply); err != nil {
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
		string(contentpolicy.DecisionsJSON(ev.PolicyDecisions)), cmp.Or(ev.Origin, OriginCaller),
		ev.ParentRequestID, ev.EndUserDigest, ev.BudgetBoundary, ev.PayloadCaptured); err != nil {
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
func insertFact(ctx context.Context, tx pgx.Tx, ev *Event, attempt ValidatedAttempt) (factCharge, error) {
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
			return factCharge{}, err
		}
	}
	usageComplete := status == chargeNotBillable || usage.Complete
	// A provider that answered successfully but reported no usage has been paid
	// for something this installation cannot quantify; it counts as unpriced
	// rather than as free.
	successfulWithoutUsage := !usage.Observed && attempt.Attempt.ErrorClass == nil &&
		attempt.StatusCode != nil && *attempt.StatusCode >= 200 && *attempt.StatusCode <= 299
	unpriced := status != chargeNotBillable && (successfulWithoutUsage || !pricing.complete)
	var selector, baselineCost *string
	if routing := attempt.Attempt.Routing; routing != nil {
		selector = routing.Selector
		if routing.Baseline != nil && pricing.estimatedCost != nil {
			var err error
			if baselineCost, err = priceBaseline(ctx, tx, ev, attempt); err != nil {
				return factCharge{}, err
			}
		}
	}
	if _, err := tx.Exec(ctx, insertFactSQL,
		attempt.Attempt.ID, ev.EventID, ev.RequestID, ev.RequestStartedAt, attempt.Ordinal,
		ev.APIKeyID, attempt.Attempt.ProviderID, ev.RouteSlug, attempt.Attempt.UpstreamModel,
		ev.Operation, ev.Surface, ev.ObservedAt, status, usage.Observed, usageComplete,
		usage.InputTokens, usage.OutputTokens, usage.CachedInputTokens, usage.MediaUnits,
		pricing.estimatedCost, unpriced, pricing.pricingRevisionID, pricing.currency,
		ev.BudgetGroupID, usage.CacheWriteInputTokens, usage.CacheWrite5MInputTokens,
		usage.CacheWrite1HInputTokens, string(AttributionJSON(ev.Attribution)),
		attempt.Attempt.EstimatedInputTokens, attempt.Attempt.EstimateProvenance, attempt.Attempt.ModelFamily,
		selector, baselineCost, ev.EndUserDigest, attempt.Attempt.Routing != nil && attempt.Attempt.Routing.BudgetExempt,
	); err != nil {
		return factCharge{}, fmt.Errorf("persist attempt usage fact: %w", err)
	}
	return factCharge{billable: status != chargeNotBillable, cost: pricing.estimatedCost, unpriced: unpriced}, nil
}

// factCharge is what one attempt fact may be charged: its estimated cost, nil
// when it has none, and whether it is billable without a price.
type factCharge struct {
	billable bool
	cost     *string
	unpriced bool
}

// chargeSupply folds a billable attempt's charge into the spend windows of
// every capped route, connection and slot it was dispatched against, keeping
// each owner's latest balance. The balances name the request, so installing
// them also removes what the gateway reserved against those caps.
func chargeSupply(ctx context.Context, tx pgx.Tx, ev *Event, attempt ValidatedAttempt, charge factCharge, balances map[string]limits.CostSnapshot) error {
	if !charge.billable || attempt.Attempt.Routing == nil {
		return nil
	}
	cost, unpriced := "0", int64(0)
	if charge.cost != nil {
		cost = *charge.cost
	}
	if charge.unpriced {
		unpriced = 1
	}
	for _, owner := range attempt.Attempt.Routing.Budgets {
		snapshot, err := limits.AddSupplyCostDelta(ctx, tx, owner, ev.ObservedAt, cost, unpriced)
		if err != nil {
			return fmt.Errorf("apply supply cost delta: %w", err)
		}
		snapshot.RequestID = ev.RequestID
		balances[owner] = snapshot
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
	// Keyless requests are the installation's own: they spend from no key's
	// or group's budget.
	endUsers, err := applyIdentityCostDelta(ctx, tx, ev, cost, unpriced)
	if err != nil {
		return nil, err
	}

	if facts == 0 || ev.APIKeyID == "" {
		return endUsers, nil
	}
	snapshot, err := limits.AddCostDelta(ctx, tx, ev.APIKeyID, ev.ObservedAt, cost, unpriced)
	if err != nil {
		return nil, fmt.Errorf("apply request metadata cost delta: %w", err)
	}
	// Each snapshot names the request it accounts for, so that installing it in
	// the distributed counters also removes the cost the gateway reserved for the
	// request when it was admitted.
	snapshot.RequestID = ev.RequestID
	snapshots := append([]limits.CostSnapshot{snapshot}, endUsers...)
	if ev.BudgetGroupID != nil {
		group, err := limits.AddGroupCostDelta(ctx, tx, *ev.BudgetGroupID, ev.ObservedAt, cost, unpriced)
		if err != nil {
			return nil, fmt.Errorf("apply request metadata group cost delta: %w", err)
		}
		group.RequestID = ev.RequestID
		snapshots = append(snapshots, group)
	}
	return snapshots, nil
}

// A refusal before dispatch still establishes the durable current-window
// balance. It must not assume zero when this identity already has usage.
func applyEndUserCostDelta(ctx context.Context, tx pgx.Tx, ev *Event, cost string, unpriced int64) ([]limits.CostSnapshot, error) {
	if ev.EndUserDigest == "" || ev.APIKeyID == "" {
		return nil, nil
	}
	snapshots, err := limits.AddEndUserCostDelta(ctx, tx, ev.APIKeyID, ev.EndUserDigest, ev.ObservedAt, cost, unpriced)
	if err != nil {
		return nil, fmt.Errorf("apply end-user cost delta: %w", err)
	}
	for i := range snapshots {
		snapshots[i].RequestID = ev.RequestID
	}
	return snapshots, nil
}

func applyIdentityCostDelta(ctx context.Context, tx pgx.Tx, ev *Event, cost string, unpriced int64) ([]limits.CostSnapshot, error) {
	snapshots, err := applyEndUserCostDelta(ctx, tx, ev, cost, unpriced)
	if err != nil {
		return nil, err
	}
	aggregates, err := limits.AddAggregateCostDelta(ctx, tx, ev.APIKeyID, ev.ProviderID, ev.RouteSlug, ev.Attribution, ev.ObservedAt, cost, unpriced)
	if err != nil {
		return nil, fmt.Errorf("apply aggregate cost delta: %w", err)
	}
	for i := range aggregates {
		aggregates[i].RequestID = ev.RequestID
	}
	return append(snapshots, aggregates...), nil
}
