use super::super::validation::ValidatedAttempt;
use super::{AttemptChargeStatus, PersistedAttemptFact};
use crate::error::Error;
use olp_engine::inference::request_metadata::Event;
use uuid::Uuid;

struct AttemptPricing {
    pricing_revision_id: Option<Uuid>,
    currency: Option<String>,
    pricing_complete: bool,
    estimated_cost: Option<rust_decimal::Decimal>,
}
async fn price_attempt(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event: &Event,
    attempt: &ValidatedAttempt<'_>,
) -> Result<AttemptPricing, Error> {
    // `$5` (input_tokens) is cache-inclusive, so the uncached portion
    // is `$5 - $9`. When the revision carries no cached tier the whole
    // input count keeps billing at the full input rate, exactly as it
    // did before the tier existed — history stays comparable.
    let pricing = sqlx::query!(
                "SELECT selected.pricing_revision_id AS \"pricing_revision_id?\", \
                        selected.currency AS \"currency?\", \
                        selected.pricing_revision_id IS NOT NULL \
                          AND ($5::bigint IS NULL OR selected.input_per_million IS NOT NULL) \
                          AND ($6::bigint IS NULL OR selected.output_per_million IS NOT NULL) \
                          AND ($7::numeric IS NULL OR selected.unit_price IS NOT NULL) \
                          AS \"pricing_complete!\", \
                        CASE WHEN $8::boolean \
                                   AND selected.pricing_revision_id IS NOT NULL \
                                   AND ($5::bigint IS NULL OR selected.input_per_million IS NOT NULL) \
                                   AND ($6::bigint IS NULL OR selected.output_per_million IS NOT NULL) \
                                   AND ($7::numeric IS NULL OR selected.unit_price IS NOT NULL) \
                             THEN (COALESCE(( \
                                       CASE WHEN selected.cached_input_per_million IS NULL \
                                            THEN $5::numeric * selected.input_per_million \
                                            ELSE GREATEST($5::numeric \
                                                   - COALESCE($9::bigint, 0), 0) \
                                                 * selected.input_per_million \
                                               + LEAST(COALESCE($9::bigint, 0)::numeric, \
                                                       $5::numeric) \
                                                 * selected.cached_input_per_million \
                                       END) / 1000000, 0) \
                                 + COALESCE($6::numeric * selected.output_per_million / 1000000, 0) \
                                 + COALESCE($7::numeric * selected.unit_price, 0)) \
                             ELSE NULL END AS \"estimated_cost?\" \
                 FROM providers provider \
                 LEFT JOIN LATERAL ( \
                     SELECT revision.id AS pricing_revision_id, price.input_per_million, \
                            price.cached_input_per_million, \
                            price.output_per_million, price.unit_price, \
                            price.currency::text AS currency \
                     FROM pricing_revisions revision \
                     JOIN prices price ON price.pricing_revision_id = revision.id \
                     WHERE revision.effective_at <= $4 \
                       AND price.provider_kind = provider.kind \
                       AND (price.provider_id IS NULL OR price.provider_id = provider.id) \
                       AND price.model = $2 AND price.operation = $3 \
                     ORDER BY (price.provider_id IS NOT NULL) DESC, \
                              revision.effective_at DESC, revision.revision DESC LIMIT 1 \
                 ) selected ON true \
                 WHERE provider.id = $1",
                attempt.event.provider_id,
                &attempt.event.upstream_model,
                event.operation.as_str(),
                event.observed_at,
                attempt.usage.input_tokens,
                attempt.usage.output_tokens,
                attempt.usage.media_units,
                attempt.usage.complete,
                attempt.usage.cached_input_tokens
            )
            .fetch_one(&mut **transaction)
            .await?;
    Ok(AttemptPricing {
        pricing_revision_id: pricing.pricing_revision_id,
        currency: pricing.currency.map(|value| value.trim().to_owned()),
        pricing_complete: pricing.pricing_complete,
        estimated_cost: pricing.estimated_cost,
    })
}

pub(super) async fn insert_attempt_usage_fact<'a>(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event: &'a Event,
    attempt: &ValidatedAttempt<'a>,
) -> Result<Option<PersistedAttemptFact<'a>>, Error> {
    let charge_status = if attempt.usage.billing_uncertain {
        AttemptChargeStatus::BillingUncertain
    } else if attempt.usage.observed {
        AttemptChargeStatus::Billable
    } else {
        AttemptChargeStatus::NotBillable
    };
    let AttemptPricing {
        pricing_revision_id,
        currency,
        pricing_complete,
        estimated_cost,
    } = if charge_status == AttemptChargeStatus::NotBillable {
        AttemptPricing {
            pricing_revision_id: None,
            currency: None,
            pricing_complete: true,
            estimated_cost: None,
        }
    } else {
        price_attempt(transaction, event, attempt).await?
    };
    let usage_complete =
        charge_status == AttemptChargeStatus::NotBillable || attempt.usage.complete;
    let successful_without_usage = !attempt.usage.observed
        && attempt.event.error_class.is_none()
        && matches!(attempt.event.status_code, Some(200..=299));
    let unpriced = charge_status != AttemptChargeStatus::NotBillable
        && (successful_without_usage || !pricing_complete);

    let inserted = sqlx::query!(
        "INSERT INTO attempt_usage_facts \
             (attempt_id, event_id, request_id, request_started_at, attempt_ordinal, \
              api_key_id, provider_id, route_slug, upstream_model, operation, surface, \
              observed_at, charge_status, \
              usage_observed, usage_complete, input_tokens, output_tokens, \
              cached_input_tokens, media_units, estimated_cost, unpriced, \
              pricing_revision_id, currency, request_counted, provider_request_counted, \
              model_request_counted, target_request_counted, request_unpriced_counted, \
              provider_unpriced_counted, model_unpriced_counted, target_unpriced_counted, \
              request_incomplete_counted, provider_incomplete_counted, \
              model_incomplete_counted, target_incomplete_counted) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, \
                     $13::text::attempt_charge_status, $14, $15, $16, $17, $18, $19, \
                     $20::numeric, $21, $22, $23, false, false, false, false, false, \
                     false, false, false, false, false, false, false) \
             ON CONFLICT (request_id, attempt_ordinal) DO NOTHING",
        attempt.event.id,
        event.event_id,
        event.request_id,
        event.request_started_at,
        attempt.ordinal,
        event.api_key_id,
        attempt.event.provider_id,
        &event.route_slug,
        &attempt.event.upstream_model,
        event.operation.as_str(),
        event.surface.as_str(),
        event.observed_at,
        charge_status.as_str(),
        attempt.usage.observed,
        usage_complete,
        attempt.usage.input_tokens,
        attempt.usage.output_tokens,
        attempt.usage.cached_input_tokens,
        attempt.usage.media_units,
        estimated_cost,
        unpriced,
        pricing_revision_id,
        currency.as_deref()
    )
    .execute(&mut **transaction)
    .await?;
    if inserted.rows_affected() == 0 {
        return Ok(None);
    }
    Ok(Some(PersistedAttemptFact {
        attempt: attempt.event,
        usage: attempt.usage.clone(),
        charge_status,
        estimated_cost,
        unpriced,
        pricing_revision_id,
        currency,
    }))
}
