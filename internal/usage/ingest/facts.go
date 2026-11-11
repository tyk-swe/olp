package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/ledger"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/pricebook"
)

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
         estimated_input_tokens, estimate_provenance, model_family, selector, baseline_cost, end_user_digest, budget_exempt,
         audio_input_tokens, cached_audio_input_tokens, audio_output_tokens,
         image_input_tokens, cached_image_input_tokens, image_output_tokens,
         tool_calls, batch, pricing_components,
         commitment_weight, commitment_amount, commitment_period,
         discount_id, charge, rate_card_revision_id,
         list_cost, contracted_cost, caller_credential, cost_lines, charge_lines, commitment_schedule_id, commitment_bucket, training_tokens, storage_byte_seconds, cache_token_seconds, container_sessions, invocation_count, agent_principal_id, delegation_pair_digest, member_principal_id)
    VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, NULLIF($6, '')::uuid, $24::uuid, $7::uuid, $8, $9, $10, $11, $12,
            $13, $14, $15, $16, $17, $18, $25, $26, $27, $19::numeric, $20::numeric, $21, $22::uuid, $23,
            false, false, false, false, false, false, false, false, false, false, false, false, $28::jsonb,
            $29, $30, $31, $32, $33::numeric, $34, $35,
            $36, $37, $38, $39, $40, $41, $42::jsonb, $43, $44::text[],
            $45::numeric, $46::numeric, $47,
            $48::uuid, $49::numeric, $50::uuid,
            $51::numeric, $52::numeric, $53, $54::jsonb, $55::jsonb, $56::uuid, $57::timestamptz, $58, $59::text::numeric, $60::text::numeric, $61::jsonb, $62, NULLIF($63, '')::uuid, NULLIF($64, ''), NULLIF($65, '')::uuid)
    ON CONFLICT (request_id, attempt_ordinal) DO NOTHING`

// factTotalsSQL sums exactly the facts this event inserted. The receipt
// admission guarantees no other event's facts share this event identifier, and
// numeric keeps the sum exact.
const factTotalsSQL = `SELECT count(*), COALESCE(sum(COALESCE(estimated_cost,0)+cache_budget_cost) FILTER (WHERE NOT budget_exempt), 0)::text,
        count(*) FILTER (WHERE NOT budget_exempt AND charge_status <> 'not_billable' AND unpriced),
        COALESCE(sum(COALESCE(charge,0)+cache_budget_charge) FILTER (WHERE NOT budget_exempt), 0)::text,
        count(*) FILTER (WHERE NOT budget_exempt AND charge_status <> 'not_billable' AND charge IS NULL)
    FROM olp.attempt_usage_facts WHERE event_id = $1::uuid AND ($2::uuid IS NULL OR attempt_id = $2::uuid)`

// insertFact classifies one attempt and records what it may be charged for. An
// attempt that never reached a provider is not billable and carries no price; an
// attempt whose evidence never arrived is billing uncertain, priced if it can
// be, and never counted as complete.
func insertFact(ctx context.Context, tx pgx.Tx, ev *usage.Event, attempt usage.ValidatedAttempt) (factCharge, error) {
	usage := attempt.Usage
	status := chargeNotBillable
	switch {
	case usage.Evidence.Uncertain():
		status = chargeBillingUncertain
	case usage.Evidence.Observed():
		status = chargeBillable
	}
	pricing := pricebook.AttemptPricing{Complete: true}
	if status != chargeNotBillable {
		var err error
		if pricing, err = pricebook.PriceAttempt(ctx, tx, ev, attempt); err != nil {
			return factCharge{}, err
		}
	}
	usageComplete := status == chargeNotBillable || usage.Evidence.Complete()
	// A provider that answered successfully but reported no usage has been paid
	// for something this installation cannot quantify; it counts as unpriced
	// rather than as free.
	successfulWithoutUsage := !usage.Evidence.Observed() && attempt.Attempt.ErrorClass == nil &&
		attempt.StatusCode != nil && *attempt.StatusCode >= 200 && *attempt.StatusCode <= 299
	// Incomplete usage is never given a cost above, so a billable attempt
	// with incomplete usage is unpriced spend, whatever rates exist.
	unpriced := status != chargeNotBillable && (successfulWithoutUsage || !pricing.Complete || !usage.Evidence.Complete())
	var selector, baselineCost *string
	if routing := attempt.Attempt.Routing; routing != nil {
		selector = routing.Selector
		if routing.Baseline != nil && pricing.EstimatedCost != nil {
			var err error
			if baselineCost, err = pricebook.PriceBaseline(ctx, tx, ev, attempt); err != nil {
				return factCharge{}, err
			}
		}
	}
	toolCalls := any("{}")
	if len(usage.ToolCalls) > 0 {
		data, err := json.Marshal(usage.ToolCalls)
		if err != nil {
			return factCharge{}, fmt.Errorf("persist attempt usage fact: %w", err)
		}
		toolCalls = string(data)
	}
	estimatedTokens, estimateProvenance := attempt.Attempt.InputEstimate.Columns()
	if _, err := tx.Exec(ctx, insertFactSQL,
		attempt.Attempt.ID, ev.EventID, ev.RequestID, ev.RequestStartedAt, attempt.Ordinal,
		ev.APIKeyID, attempt.Attempt.ProviderID, ev.RouteSlug, attempt.Attempt.UpstreamModel,
		ev.Operation, ev.Surface, ev.ObservedAt, status, usage.Evidence.Observed(), usageComplete,
		usage.InputTokens, usage.OutputTokens, usage.CachedInputTokens, usage.MediaUnits,
		pricing.EstimatedCost, unpriced, pricing.PricingRevisionID, pricing.Currency,
		ev.BudgetGroupID, usage.CacheWriteInputTokens, usage.CacheWrite5MInputTokens,
		usage.CacheWrite1HInputTokens, string(attribution.JSON(ev.Attribution)),
		estimatedTokens, estimateProvenance, attempt.Attempt.ModelFamily,
		selector, baselineCost, ev.EndUserDigest, attempt.Attempt.Routing != nil && attempt.Attempt.Routing.BudgetExempt,
		usage.AudioInputTokens, usage.CachedAudioInputTokens, usage.AudioOutputTokens,
		usage.ImageInputTokens, usage.CachedImageInputTokens, usage.ImageOutputTokens,
		toolCalls, usage.Batch, pricing.Components,
		pricing.CommitmentWeight, pricing.CommitmentAmount, pricing.CommitmentPeriod,
		pricing.DiscountID, pricing.Charge, pricing.RateCardRevisionID,
		pricing.ListCost, pricing.ContractedCost,
		attempt.Attempt.Routing != nil && attempt.Attempt.Routing.CredentialSource == "caller",
		pricing.CostLines, pricing.ChargeLines, pricing.CommitmentScheduleID, pricing.CommitmentBucket,
		usage.TrainingTokens, usage.StorageByteSeconds, usage.CacheTokenSeconds, pricebook.OptionalJSON(usage.ContainerSessions), usage.InvocationCount, ev.AgentPrincipalID, ev.DelegationPairDigest, ev.MemberPrincipalID,
	); err != nil {
		return factCharge{}, fmt.Errorf("persist attempt usage fact: %w", err)
	}
	if attempt.Attempt.Routing != nil && attempt.Attempt.Routing.Cache != nil {
		c := attempt.Attempt.Routing.Cache
		amount := func(v string) string {
			if v == "" {
				return "0"
			}
			return v
		}
		if _, err := tx.Exec(ctx, `UPDATE olp.attempt_usage_facts SET response_cache=true,cache_avoided_cost=$2::numeric,cache_budget_cost=$3::numeric,cache_budget_charge=$4::numeric WHERE attempt_id=$1::uuid`, attempt.Attempt.ID, amount(c.AvoidedCost), amount(c.BudgetCost), amount(c.BudgetCharge)); err != nil {
			return factCharge{}, err
		}
	}
	return factCharge{billable: status != chargeNotBillable, cost: pricing.EstimatedCost, unpriced: unpriced}, nil
}

// factCharge is what one attempt fact may be charged: its estimated cost, nil
// when it has none, and whether it is billable without a price.
type factCharge struct {
	billable bool
	cost     *string
	unpriced bool
}

// chargeSupply folds an attempt's cost into the spend windows of every capped
// or durably recorded owner it was dispatched against, keeping
// each owner's latest balance. The balances name the request, so installing
// them also removes what the gateway reserved against those caps.
func chargeSupply(ctx context.Context, tx pgx.Tx, ev *usage.Event, attempt usage.ValidatedAttempt, charge factCharge, balances map[string]limits.Snapshot, recordedOwners []string) error {
	if (!charge.billable || attempt.Attempt.Routing == nil) && len(recordedOwners) == 0 {
		return nil
	}
	cost, unpriced := "0", int64(0)
	if charge.cost != nil {
		cost = *charge.cost
	}
	if charge.unpriced {
		unpriced = 1
	}
	// Durable intents include uncapped owners. Their accounting cannot depend
	// on the optional list of currently capped owners in routing provenance.
	owners := slices.Clone(recordedOwners)
	if attempt.Attempt.Routing != nil {
		owners = append(owners, attempt.Attempt.Routing.Budgets...)
	}
	slices.Sort(owners)
	for _, owner := range slices.Compact(owners) {
		snapshots, err := ledger.AddDelta(ctx, tx, limits.OwnerSupply, owner, ledger.Delta{ObservedAt: ev.ObservedAt, Cost: cost, Unpriced: unpriced, Charge: "0"})
		if err != nil {
			return fmt.Errorf("apply supply cost delta: %w", err)
		}
		for _, snapshot := range named(snapshots, ev.RequestID) {
			balances[owner] = snapshot
		}
	}
	return nil
}

// applyCostDelta folds what this event cost into the key's spend windows and
// returns the reconstructed snapshot for the distributed counters. Unpriced
// billable attempts accrue no money but are counted, so an operator can see
// that a budget is being consumed by spend nobody can price.
func applyCostDelta(ctx context.Context, tx pgx.Tx, ev *usage.Event) ([]limits.Snapshot, error) {
	return applyAttemptCostDelta(ctx, tx, ev, nil)
}

func applyAttemptCostDelta(ctx context.Context, tx pgx.Tx, ev *usage.Event, attemptID *string) ([]limits.Snapshot, error) {
	var facts int64
	delta := ledger.Delta{ObservedAt: ev.ObservedAt}
	if err := tx.QueryRow(ctx, factTotalsSQL, ev.EventID, attemptID).Scan(&facts, &delta.Cost, &delta.Unpriced, &delta.Charge, &delta.Uncharged); err != nil {
		return nil, fmt.Errorf("total attempt usage facts: %w", err)
	}
	// Keyless requests are the installation's own: they spend from no key's
	// or group's budget.
	endUsers, err := applyAttemptIdentityCostDelta(ctx, tx, ev, delta, attemptID)
	if err != nil {
		return nil, err
	}

	if facts == 0 || ev.APIKeyID == "" {
		return endUsers, nil
	}
	snapshot, err := ledger.AddDelta(ctx, tx, limits.OwnerKey, ev.APIKeyID, delta)
	if err != nil {
		return nil, fmt.Errorf("apply request metadata cost delta: %w", err)
	}
	agent, err := applyAgentCostDelta(ctx, tx, ev, delta)
	if err != nil {
		return nil, err
	}
	// Each snapshot names the request it accounts for, so that installing it in
	// the distributed counters also removes the cost the gateway reserved for the
	// request when it was admitted. The reservation can sit under either basis,
	// so both snapshots carry the identifier.
	snapshots := slices.Concat(named(slices.Concat(snapshot, agent), ev.RequestID), endUsers)
	if ev.BudgetGroupID == nil {
		return snapshots, nil
	}
	group, err := ledger.AddDelta(ctx, tx, limits.OwnerGroup, *ev.BudgetGroupID, delta)
	if err != nil {
		return nil, fmt.Errorf("apply request metadata group cost delta: %w", err)
	}
	return append(snapshots, named(group, ev.RequestID)...), nil
}

func applyAgentCostDelta(ctx context.Context, tx pgx.Tx, ev *usage.Event, delta ledger.Delta) ([]limits.Snapshot, error) {
	if ev.AgentPrincipalID == "" {
		return nil, nil
	}
	snapshots, err := ledger.AddDelta(ctx, tx, limits.OwnerKey, ev.AgentPrincipalID, delta)
	if err != nil {
		return nil, fmt.Errorf("apply agent cost delta: %w", err)
	}
	return snapshots, nil
}

// A refusal before dispatch still establishes the durable current-window
// balance. It must not assume zero when this identity already has usage.
func applyEndUserCostDelta(ctx context.Context, tx pgx.Tx, ev *usage.Event, delta ledger.Delta) ([]limits.Snapshot, error) {
	if ev.EndUserDigest == "" || ev.APIKeyID == "" {
		return nil, nil
	}
	snapshots, err := ledger.AddEndUserDelta(ctx, tx, ev.APIKeyID, ev.EndUserDigest, delta)
	if err != nil {
		return nil, fmt.Errorf("apply end-user cost delta: %w", err)
	}
	return named(snapshots, ev.RequestID), nil
}

func applyAttemptIdentityCostDelta(ctx context.Context, tx pgx.Tx, ev *usage.Event, delta ledger.Delta, attemptID *string) ([]limits.Snapshot, error) {
	snapshots, err := applyEndUserCostDelta(ctx, tx, ev, delta)
	if err != nil {
		return nil, err
	}
	aggregates, err := ledger.AddAggregateDeltas(ctx, tx, ledger.AggregateEvent{
		EventID: ev.EventID, AttemptID: attemptID, KeyID: ev.APIKeyID, ProviderID: ev.ProviderID,
		Route: ev.RouteSlug, Labels: ev.Attribution, At: ev.ObservedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("apply aggregate cost delta: %w", err)
	}
	return append(snapshots, named(aggregates, ev.RequestID)...), nil
}
