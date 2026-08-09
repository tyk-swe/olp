use chrono::{DateTime, Utc};
use olp_domain::{OperationKind, Surface};
use rust_decimal::Decimal;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use uuid::Uuid;

use super::{
    REQUEST_METADATA_EVENT_FUTURE_SKEW_MINUTES, REQUEST_METADATA_EVENT_REPLAY_HORIZON_DAYS,
};
use crate::{PersistenceError, PgStore};

/// Metadata-only request envelope. Content-bearing fields do not exist in this
/// type, making accidental prompt/output persistence structurally impossible.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RequestMetadataEvent {
    pub event_id: Uuid,
    pub request_id: Uuid,
    pub runtime_generation_id: Uuid,
    pub api_key_id: Uuid,
    /// Absent when an authenticated request fails before a provider attempt can
    /// be selected. Such events still produce request metadata, but never a
    /// usage fact.
    pub provider_id: Option<Uuid>,
    pub route_slug: String,
    pub upstream_model: Option<String>,
    pub operation: OperationKind,
    pub surface: Surface,
    pub request_started_at: DateTime<Utc>,
    pub request_completed_at: DateTime<Utc>,
    pub observed_at: DateTime<Utc>,
    pub status_code: Option<u16>,
    pub error_class: Option<String>,
    pub committed: bool,
    pub latency_ms: u64,
    pub first_byte_ms: Option<u64>,
    pub input_tokens: Option<i64>,
    pub output_tokens: Option<i64>,
    pub cached_input_tokens: Option<i64>,
    pub media_units: Option<Decimal>,
    pub usage_complete: bool,
    pub unpriced: bool,
    pub attempts: Vec<RequestAttemptMetadata>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RequestAttemptMetadata {
    pub id: Uuid,
    pub ordinal: u16,
    pub provider_id: Uuid,
    pub upstream_model: String,
    pub started_at: DateTime<Utc>,
    pub completed_at: DateTime<Utc>,
    pub status_code: Option<u16>,
    pub error_class: Option<String>,
    pub committed: bool,
    pub latency_ms: u64,
    pub first_byte_ms: Option<u64>,
    /// Attempt-local usage and billing evidence. This is optional only for
    /// events serialized by pre-0033 binaries; new producers always populate
    /// it so storage never has to attach request-level usage to the last
    /// provider by convention.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub usage: Option<RequestAttemptUsageMetadata>,
}

/// Content-free usage evidence captured for one provider attempt. Pricing is
/// deliberately resolved only while persisting the event, against the
/// immutable pricing revision effective at `observed_at`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RequestAttemptUsageMetadata {
    pub observed: bool,
    pub complete: bool,
    pub billing_uncertain: bool,
    pub input_tokens: Option<i64>,
    pub output_tokens: Option<i64>,
    pub cached_input_tokens: Option<i64>,
    pub media_units: Option<Decimal>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RequestMetadataPersistenceOutcome {
    Persisted,
    Duplicate,
    RejectedOutsideReplayWindow,
}

struct ValidatedAttempt<'a> {
    event: &'a RequestAttemptMetadata,
    usage: ValidatedAttemptUsage,
    ordinal: i16,
    status_code: Option<i32>,
    latency_ms: i32,
    first_byte_ms: Option<i32>,
}

#[derive(Clone)]
struct ValidatedAttemptUsage {
    observed: bool,
    complete: bool,
    billing_uncertain: bool,
    input_tokens: Option<i64>,
    output_tokens: Option<i64>,
    cached_input_tokens: Option<i64>,
    media_units: Option<Decimal>,
}

#[derive(Clone, Copy, Eq, PartialEq)]
enum AttemptChargeStatus {
    NotBillable,
    Billable,
    BillingUncertain,
}

impl AttemptChargeStatus {
    const fn as_str(self) -> &'static str {
        match self {
            Self::NotBillable => "not_billable",
            Self::Billable => "billable",
            Self::BillingUncertain => "billing_uncertain",
        }
    }
}

struct PersistedAttemptFact<'a> {
    attempt: &'a RequestAttemptMetadata,
    usage: ValidatedAttemptUsage,
    charge_status: AttemptChargeStatus,
    estimated_cost: Option<Decimal>,
    unpriced: bool,
    pricing_revision_id: Option<Uuid>,
    currency: Option<String>,
}

struct ValidatedRequestMetadata<'a> {
    has_attempts: bool,
    status_code: Option<i32>,
    latency_ms: i32,
    first_byte_ms: Option<i32>,
    attempt_count: i16,
    attempts: Vec<ValidatedAttempt<'a>>,
}

impl<'a> ValidatedRequestMetadata<'a> {
    fn validate(event: &'a RequestMetadataEvent) -> Result<Self, PersistenceError> {
        let has_attempts = !event.attempts.is_empty();
        let final_target_matches = event.attempts.last().is_none_or(|attempt| {
            event.provider_id == Some(attempt.provider_id)
                && event.upstream_model.as_deref() == Some(attempt.upstream_model.as_str())
                && attempt.committed == event.committed
        });
        let empty_attempt_metadata_is_valid = has_attempts
            || (event.provider_id.is_none()
                && event.upstream_model.is_none()
                && !event.committed
                && event.first_byte_ms.is_none()
                && event.input_tokens.is_none()
                && event.output_tokens.is_none()
                && event.cached_input_tokens.is_none()
                && event.media_units.is_none()
                && !event.usage_complete);
        let has_attempt_usage = event.attempts.iter().any(|attempt| attempt.usage.is_some());
        let attempt_usage_shape_is_valid =
            !has_attempt_usage || event.attempts.iter().all(|attempt| attempt.usage.is_some());
        if event.request_completed_at < event.request_started_at
            || event.route_slug.trim().is_empty()
            || !valid_status(event.status_code)
            || !final_target_matches
            || !empty_attempt_metadata_is_valid
            || !attempt_usage_shape_is_valid
        {
            return Err(PersistenceError::InvalidRequestMetadataEvent);
        }

        let attempts = event
            .attempts
            .iter()
            .enumerate()
            .map(|(index, attempt)| {
                if usize::from(attempt.ordinal) != index + 1
                    || attempt.completed_at < attempt.started_at
                    || !valid_status(attempt.status_code)
                {
                    return Err(PersistenceError::InvalidRequestMetadataEvent);
                }
                let usage = validated_attempt_usage(event, attempt, index)?;
                Ok(ValidatedAttempt {
                    event: attempt,
                    usage,
                    ordinal: i16::try_from(attempt.ordinal)
                        .map_err(|_| PersistenceError::InvalidRequestMetadataEvent)?,
                    status_code: attempt.status_code.map(i32::from),
                    latency_ms: checked_milliseconds(attempt.latency_ms)?,
                    first_byte_ms: attempt
                        .first_byte_ms
                        .map(checked_milliseconds)
                        .transpose()?,
                })
            })
            .collect::<Result<Vec<_>, _>>()?;

        Ok(Self {
            has_attempts,
            status_code: event.status_code.map(i32::from),
            latency_ms: checked_milliseconds(event.latency_ms)?,
            first_byte_ms: event.first_byte_ms.map(checked_milliseconds).transpose()?,
            attempt_count: i16::try_from(attempts.len())
                .map_err(|_| PersistenceError::InvalidRequestMetadataEvent)?,
            attempts,
        })
    }
}

fn validated_attempt_usage(
    request: &RequestMetadataEvent,
    attempt: &RequestAttemptMetadata,
    index: usize,
) -> Result<ValidatedAttemptUsage, PersistenceError> {
    let usage = attempt
        .usage
        .as_ref()
        .map(|usage| ValidatedAttemptUsage {
            observed: usage.observed,
            complete: usage.complete,
            billing_uncertain: usage.billing_uncertain,
            input_tokens: usage.input_tokens,
            output_tokens: usage.output_tokens,
            cached_input_tokens: usage.cached_input_tokens,
            media_units: usage.media_units,
        })
        .unwrap_or_else(|| {
            let is_final = index + 1 == request.attempts.len();
            if is_final {
                let observed = request.usage_complete
                    || request.input_tokens.is_some()
                    || request.output_tokens.is_some()
                    || request.cached_input_tokens.is_some()
                    || request.media_units.is_some();
                ValidatedAttemptUsage {
                    observed,
                    complete: request.usage_complete,
                    billing_uncertain: !observed && legacy_attempt_may_be_billable(attempt),
                    input_tokens: request.input_tokens,
                    output_tokens: request.output_tokens,
                    cached_input_tokens: request.cached_input_tokens,
                    media_units: request.media_units,
                }
            } else {
                let billing_uncertain = legacy_attempt_may_be_billable(attempt);
                ValidatedAttemptUsage {
                    observed: false,
                    complete: !billing_uncertain,
                    billing_uncertain,
                    input_tokens: None,
                    output_tokens: None,
                    cached_input_tokens: None,
                    media_units: None,
                }
            }
        });
    let state_is_valid = matches!(
        (usage.observed, usage.complete, usage.billing_uncertain),
        (false, true, false) | (false, false, true) | (true, _, false)
    );
    if !state_is_valid
        || !usage.observed
            && (usage.input_tokens.is_some()
                || usage.output_tokens.is_some()
                || usage.cached_input_tokens.is_some()
                || usage.media_units.is_some())
        || usage.input_tokens.is_some_and(|value| value < 0)
        || usage.output_tokens.is_some_and(|value| value < 0)
        || usage.cached_input_tokens.is_some_and(|value| value < 0)
        || usage.media_units.is_some_and(|value| value < Decimal::ZERO)
    {
        return Err(PersistenceError::InvalidRequestMetadataEvent);
    }
    Ok(usage)
}

fn legacy_attempt_may_be_billable(attempt: &RequestAttemptMetadata) -> bool {
    attempt.committed
        || matches!(
            attempt.error_class.as_deref(),
            Some("ambiguous" | "timeout" | "upstream_server" | "protocol" | "cancelled")
        )
}

const fn valid_status(status: Option<u16>) -> bool {
    match status {
        Some(status) => status >= 100 && status <= 599,
        None => true,
    }
}

fn checked_milliseconds(value: u64) -> Result<i32, PersistenceError> {
    i32::try_from(value).map_err(|_| PersistenceError::InvalidRequestMetadataEvent)
}

impl PgStore {
    /// Persists one idempotent metadata-only stream event. A bounded durable
    /// receipt protects the supported seven-day delivery window after raw
    /// facts roll into hourly usage. Older entries are rejected explicitly so
    /// they cannot silently add to an aggregate after their receipt expires.
    pub async fn persist_request_metadata_event(
        &self,
        event: &RequestMetadataEvent,
    ) -> Result<RequestMetadataPersistenceOutcome, PersistenceError> {
        let event_sha256: [u8; 32] = Sha256::digest(serde_json::to_vec(event)?).into();
        self.persist_request_metadata_event_with_digest(event, event_sha256)
            .await
    }

    /// Processes an event decoded from a Valkey Stream while fingerprinting
    /// the original bytes. Replays therefore remain stable across application
    /// versions even if Rust's serialization of [`RequestMetadataEvent`] later changes.
    pub async fn persist_request_metadata_stream_event(
        &self,
        event: &RequestMetadataEvent,
        original_payload: &[u8],
    ) -> Result<RequestMetadataPersistenceOutcome, PersistenceError> {
        let event_sha256: [u8; 32] = Sha256::digest(original_payload).into();
        self.persist_request_metadata_event_with_digest(event, event_sha256)
            .await
    }

    async fn persist_request_metadata_event_with_digest(
        &self,
        event: &RequestMetadataEvent,
        event_sha256: [u8; 32],
    ) -> Result<RequestMetadataPersistenceOutcome, PersistenceError> {
        let validated = ValidatedRequestMetadata::validate(event)?;
        let mut transaction = self.pool().begin().await?;
        let receipt: Option<Uuid> = sqlx::query_scalar!(
            "INSERT INTO request_metadata_event_receipts \
             (event_id, request_id, event_sha256, status, observed_at) \
             SELECT $1, $2, $3, 'pending'::request_metadata_event_receipt_status, $4 \
             WHERE $4 >= now() - make_interval(days => $5) \
               AND $4 <= now() + make_interval(mins => $6) \
               AND NOT EXISTS (SELECT 1 FROM usage_facts \
                               WHERE id = $1 OR request_id = $2) \
               AND NOT EXISTS (SELECT 1 FROM attempt_usage_facts \
                               WHERE event_id = $1 OR request_id = $2) \
             ON CONFLICT DO NOTHING RETURNING event_id",
            event.event_id,
            event.request_id,
            event_sha256.as_slice(),
            event.observed_at,
            REQUEST_METADATA_EVENT_REPLAY_HORIZON_DAYS,
            REQUEST_METADATA_EVENT_FUTURE_SKEW_MINUTES
        )
        .fetch_optional(&mut *transaction)
        .await?;
        if receipt.is_none() {
            let existing = sqlx::query!(
                "SELECT \
                   EXISTS (SELECT 1 FROM request_metadata_event_receipts \
                           WHERE event_id = $1 AND request_id = $2) AS \"receipt_exists!\", \
                   (SELECT event_sha256 FROM request_metadata_event_receipts \
                    WHERE event_id = $1 AND request_id = $2) AS event_sha256, \
                   EXISTS (SELECT 1 FROM usage_facts \
                           WHERE id = $1 AND request_id = $2) AS \"fact_exists!\", \
                   EXISTS (SELECT 1 FROM attempt_usage_facts \
                           WHERE event_id = $1 AND request_id = $2) AS \"attempt_fact_exists!\", \
                   ($3 < now() - make_interval(days => $4) \
                    OR $3 > now() + make_interval(mins => $5)) AS \"outside_window!\"",
                event.event_id,
                event.request_id,
                event.observed_at,
                REQUEST_METADATA_EVENT_REPLAY_HORIZON_DAYS,
                REQUEST_METADATA_EVENT_FUTURE_SKEW_MINUTES,
            )
            .fetch_one(&mut *transaction)
            .await?;
            let receipt_exists: bool = existing.receipt_exists;
            let exact_receipt = receipt_exists
                && existing
                    .event_sha256
                    .is_none_or(|stored| stored.as_slice() == event_sha256);
            let exact_raw_fact: bool = existing.fact_exists || existing.attempt_fact_exists;
            if exact_receipt || exact_raw_fact {
                transaction.rollback().await?;
                return Ok(RequestMetadataPersistenceOutcome::Duplicate);
            }
            if existing.outside_window {
                let rejection: Option<Uuid> = sqlx::query_scalar!(
                    "INSERT INTO request_metadata_event_receipts \
                     (event_id, request_id, event_sha256, status, observed_at) \
                     SELECT $1, $2, $3, 'rejected'::request_metadata_event_receipt_status, $4 \
                     WHERE NOT EXISTS (SELECT 1 FROM usage_facts \
                                       WHERE id = $1 OR request_id = $2) \
                       AND NOT EXISTS (SELECT 1 FROM attempt_usage_facts \
                                       WHERE event_id = $1 OR request_id = $2) \
                     ON CONFLICT DO NOTHING RETURNING event_id",
                    event.event_id,
                    event.request_id,
                    event_sha256.as_slice(),
                    event.observed_at
                )
                .fetch_optional(&mut *transaction)
                .await?;
                if rejection.is_some() {
                    sqlx::query!(
                        "INSERT INTO request_metadata_ingestion_gaps \
                         (id, gateway_instance, event_count, reason, certainty, \
                          first_observed_at, last_observed_at) \
                         VALUES ($1, 'request-metadata-consumer', 0, \
                                 'request_metadata_event_outside_replay_window', \
                                 'lower_bound'::request_metadata_gap_certainty, now(), now())",
                        Uuid::now_v7()
                    )
                    .execute(&mut *transaction)
                    .await?;
                    transaction.commit().await?;
                    return Ok(RequestMetadataPersistenceOutcome::RejectedOutsideReplayWindow);
                }
                let exact_after_race: bool = sqlx::query_scalar!(
                    "SELECT EXISTS ( \
                       SELECT 1 FROM request_metadata_event_receipts \
                       WHERE event_id = $1 AND request_id = $2 \
                         AND (event_sha256 IS NULL OR event_sha256 = $3) \
                       UNION ALL \
                       SELECT 1 FROM usage_facts WHERE id = $1 AND request_id = $2 \
                       UNION ALL \
                       SELECT 1 FROM attempt_usage_facts \
                        WHERE event_id = $1 AND request_id = $2 \
                     ) AS \"value!\"",
                    event.event_id,
                    event.request_id,
                    event_sha256.as_slice()
                )
                .fetch_one(&mut *transaction)
                .await?;
                transaction.rollback().await?;
                return if exact_after_race {
                    Ok(RequestMetadataPersistenceOutcome::Duplicate)
                } else {
                    Err(PersistenceError::InvalidRequestMetadataEvent)
                };
            }
            transaction.rollback().await?;
            return Err(PersistenceError::InvalidRequestMetadataEvent);
        }
        sqlx::query!(
            "INSERT INTO requests \
              (id, runtime_generation_id, api_key_id, route_slug, operation, surface, \
              started_at, completed_at, status_code, error_class, total_latency_ms, first_byte_ms, \
              attempt_count, created_at) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $8) \
             ON CONFLICT (id, started_at) DO NOTHING",
            event.request_id,
            event.runtime_generation_id,
            event.api_key_id,
            &event.route_slug,
            event.operation.as_str(),
            event.surface.as_str(),
            event.request_started_at,
            event.request_completed_at,
            validated.status_code,
            event.error_class.as_deref(),
            validated.latency_ms,
            validated.first_byte_ms,
            validated.attempt_count
        )
        .execute(&mut *transaction)
        .await?;
        for attempt in &validated.attempts {
            sqlx::query!(
                "INSERT INTO attempts \
                 (id, request_id, request_started_at, ordinal, provider_id, upstream_model, \
                  started_at, completed_at, status_code, error_class, committed, latency_ms, first_byte_ms) \
                 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) \
                 ON CONFLICT (request_id, ordinal) DO NOTHING",
            attempt.event.id, event.request_id, event.request_started_at, attempt.ordinal,
            attempt.event.provider_id, &attempt.event.upstream_model, attempt.event.started_at,
            attempt.event.completed_at, attempt.status_code, attempt.event.error_class.as_deref(),
            attempt.event.committed, attempt.latency_ms, attempt.first_byte_ms)
            .execute(&mut *transaction)
            .await?;
        }

        // Authenticated decoding, route, and capability failures are valuable
        // operational metadata, but no provider usage exists to price or roll
        // up before the first attempt begins.
        if !validated.has_attempts {
            mark_request_metadata_receipt_persisted(
                &mut transaction,
                event.event_id,
                event.request_id,
            )
            .await?;
            transaction.commit().await?;
            return Ok(RequestMetadataPersistenceOutcome::Persisted);
        }

        sqlx::query!(
            "INSERT INTO usage_request_anchors (request_id, request_started_at) \
             VALUES ($1, $2) ON CONFLICT DO NOTHING",
            event.request_id,
            event.request_started_at
        )
        .execute(&mut *transaction)
        .await?;

        let mut persisted_facts = Vec::with_capacity(validated.attempts.len());
        for attempt in &validated.attempts {
            let charge_status = if attempt.usage.billing_uncertain {
                AttemptChargeStatus::BillingUncertain
            } else if attempt.usage.observed {
                AttemptChargeStatus::Billable
            } else {
                AttemptChargeStatus::NotBillable
            };
            let (pricing_revision_id, currency, pricing_complete, estimated_cost) = if charge_status
                == AttemptChargeStatus::NotBillable
            {
                (None, None, true, None)
            } else {
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
                                     THEN (COALESCE($5::numeric * selected.input_per_million / 1000000, 0) \
                                         + COALESCE($6::numeric * selected.output_per_million / 1000000, 0) \
                                         + COALESCE($7::numeric * selected.unit_price, 0)) \
                                     ELSE NULL END AS \"estimated_cost?\" \
                         FROM providers provider \
                         LEFT JOIN LATERAL ( \
                             SELECT revision.id AS pricing_revision_id, price.input_per_million, \
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
                        attempt.usage.complete
                    )
                    .fetch_one(&mut *transaction)
                    .await?;
                (
                    pricing.pricing_revision_id,
                    pricing.currency.map(|value| value.trim().to_owned()),
                    pricing.pricing_complete,
                    pricing.estimated_cost,
                )
            };
            let usage_complete =
                charge_status == AttemptChargeStatus::NotBillable || attempt.usage.complete;
            let unpriced = charge_status != AttemptChargeStatus::NotBillable && !pricing_complete;

            sqlx::query!(
                "INSERT INTO attempt_usage_facts \
                 (attempt_id, event_id, request_id, request_started_at, attempt_ordinal, \
                  api_key_id, provider_id, route_slug, upstream_model, operation, surface, \
                  attempt_started_at, attempt_completed_at, observed_at, charge_status, \
                  usage_observed, usage_complete, input_tokens, output_tokens, \
                  cached_input_tokens, media_units, estimated_cost, unpriced, \
                  pricing_revision_id, currency, request_counted, provider_request_counted, \
                  model_request_counted, target_request_counted, request_unpriced_counted, \
                  provider_unpriced_counted, model_unpriced_counted, target_unpriced_counted, \
                  request_incomplete_counted, provider_incomplete_counted, \
                  model_incomplete_counted, target_incomplete_counted) \
                 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, \
                         $15::text::attempt_charge_status, $16, $17, $18, $19, $20, $21, \
                         $22::numeric, $23, $24, $25, false, false, false, false, false, \
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
                attempt.event.started_at,
                attempt.event.completed_at,
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
            .execute(&mut *transaction)
            .await?;
            persisted_facts.push(PersistedAttemptFact {
                attempt: attempt.event,
                usage: attempt.usage.clone(),
                charge_status,
                estimated_cost,
                unpriced,
                pricing_revision_id,
                currency,
            });
        }

        sqlx::query!(
            "WITH marked AS ( \
                 SELECT attempt_id, \
                        row_number() OVER (PARTITION BY request_id ORDER BY attempt_ordinal) = 1 \
                            AS request_marker, \
                        row_number() OVER (PARTITION BY request_id, provider_id \
                                           ORDER BY attempt_ordinal) = 1 AS provider_marker, \
                        row_number() OVER (PARTITION BY request_id, upstream_model \
                                           ORDER BY attempt_ordinal) = 1 AS model_marker, \
                        row_number() OVER (PARTITION BY request_id, provider_id, upstream_model \
                                           ORDER BY attempt_ordinal) = 1 AS target_marker, \
                        bool_or(charge_status <> 'not_billable' AND unpriced) \
                            OVER (PARTITION BY request_id) AS request_unpriced, \
                        bool_or(charge_status <> 'not_billable' AND unpriced) \
                            OVER (PARTITION BY request_id, provider_id) AS provider_unpriced, \
                        bool_or(charge_status <> 'not_billable' AND unpriced) \
                            OVER (PARTITION BY request_id, upstream_model) AS model_unpriced, \
                        bool_or(charge_status <> 'not_billable' AND unpriced) \
                            OVER (PARTITION BY request_id, provider_id, upstream_model) \
                            AS target_unpriced, \
                        bool_or(charge_status <> 'not_billable' AND NOT usage_complete) \
                            OVER (PARTITION BY request_id) AS request_incomplete, \
                        bool_or(charge_status <> 'not_billable' AND NOT usage_complete) \
                            OVER (PARTITION BY request_id, provider_id) AS provider_incomplete, \
                        bool_or(charge_status <> 'not_billable' AND NOT usage_complete) \
                            OVER (PARTITION BY request_id, upstream_model) AS model_incomplete, \
                        bool_or(charge_status <> 'not_billable' AND NOT usage_complete) \
                            OVER (PARTITION BY request_id, provider_id, upstream_model) \
                            AS target_incomplete \
                   FROM attempt_usage_facts WHERE request_id = $1 \
             ) \
             UPDATE attempt_usage_facts fact SET \
                    request_counted = marked.request_marker, \
                    provider_request_counted = marked.provider_marker, \
                    model_request_counted = marked.model_marker, \
                    target_request_counted = marked.target_marker, \
                    request_unpriced_counted = marked.request_marker AND marked.request_unpriced, \
                    provider_unpriced_counted = marked.provider_marker AND marked.provider_unpriced, \
                    model_unpriced_counted = marked.model_marker AND marked.model_unpriced, \
                    target_unpriced_counted = marked.target_marker AND marked.target_unpriced, \
                    request_incomplete_counted = \
                        marked.request_marker AND marked.request_incomplete, \
                    provider_incomplete_counted = \
                        marked.provider_marker AND marked.provider_incomplete, \
                    model_incomplete_counted = marked.model_marker AND marked.model_incomplete, \
                    target_incomplete_counted = marked.target_marker AND marked.target_incomplete \
               FROM marked WHERE fact.attempt_id = marked.attempt_id",
            event.request_id
        )
        .execute(&mut *transaction)
        .await?;

        // Keep a truthful request-level aggregate for older readers only when
        // every potentially billable attempt has the same provider/model.
        // Authoritative reads always use attempt_usage_facts.
        if let Some(first) = persisted_facts
            .iter()
            .find(|fact| fact.charge_status != AttemptChargeStatus::NotBillable)
            && persisted_facts
                .iter()
                .filter(|fact| fact.charge_status != AttemptChargeStatus::NotBillable)
                .all(|fact| {
                    fact.attempt.provider_id == first.attempt.provider_id
                        && fact.attempt.upstream_model == first.attempt.upstream_model
                })
        {
            insert_compatibility_usage_fact(&mut transaction, event, first, &persisted_facts)
                .await?;
        }
        mark_request_metadata_receipt_persisted(&mut transaction, event.event_id, event.request_id)
            .await?;
        transaction.commit().await?;
        Ok(RequestMetadataPersistenceOutcome::Persisted)
    }
}

async fn mark_request_metadata_receipt_persisted(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event_id: Uuid,
    request_id: Uuid,
) -> Result<(), PersistenceError> {
    sqlx::query!(
        "UPDATE request_metadata_event_receipts \
            SET status = 'fact_persisted'::request_metadata_event_receipt_status \
          WHERE event_id = $1 AND request_id = $2 \
            AND status = 'pending'::request_metadata_event_receipt_status",
        event_id,
        request_id
    )
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

async fn insert_compatibility_usage_fact(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event: &RequestMetadataEvent,
    attribution: &PersistedAttemptFact<'_>,
    facts: &[PersistedAttemptFact<'_>],
) -> Result<(), PersistenceError> {
    let billable = facts
        .iter()
        .filter(|fact| fact.charge_status != AttemptChargeStatus::NotBillable)
        .collect::<Vec<_>>();
    let usage_complete = billable.iter().all(|fact| fact.usage.complete);
    let unpriced = billable.iter().any(|fact| fact.unpriced);
    let input_tokens = checked_optional_i64_sum(&billable, |fact| fact.usage.input_tokens)?;
    let output_tokens = checked_optional_i64_sum(&billable, |fact| fact.usage.output_tokens)?;
    let cached_input_tokens =
        checked_optional_i64_sum(&billable, |fact| fact.usage.cached_input_tokens)?;
    let media_units = checked_optional_decimal_sum(&billable, |fact| fact.usage.media_units)?;
    let estimated_cost = if usage_complete && !unpriced {
        checked_optional_decimal_sum(&billable, |fact| fact.estimated_cost)?
    } else {
        None
    };
    let pricing_revision_id = billable
        .first()
        .map(|fact| fact.pricing_revision_id)
        .filter(|revision| {
            billable
                .iter()
                .all(|fact| fact.pricing_revision_id == *revision)
        })
        .flatten();
    let currency = billable
        .first()
        .and_then(|fact| fact.currency.as_deref())
        .filter(|currency| {
            billable
                .iter()
                .all(|fact| fact.currency.as_deref() == Some(*currency))
        });

    sqlx::query!(
        "INSERT INTO usage_facts \
         (id, request_id, request_started_at, api_key_id, provider_id, route_slug, \
          upstream_model, operation, surface, observed_at, input_tokens, output_tokens, \
          cached_input_tokens, media_units, estimated_cost, unpriced, usage_complete, \
          pricing_revision_id, currency) \
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, \
                 $15::numeric, $16, $17, $18, $19) \
         ON CONFLICT (request_id) DO NOTHING",
        event.event_id,
        event.request_id,
        event.request_started_at,
        event.api_key_id,
        attribution.attempt.provider_id,
        &event.route_slug,
        &attribution.attempt.upstream_model,
        event.operation.as_str(),
        event.surface.as_str(),
        event.observed_at,
        input_tokens,
        output_tokens,
        cached_input_tokens,
        media_units,
        estimated_cost,
        unpriced,
        usage_complete,
        pricing_revision_id,
        currency
    )
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

fn checked_optional_i64_sum<T>(
    values: &[&T],
    select: impl Fn(&T) -> Option<i64>,
) -> Result<Option<i64>, PersistenceError> {
    values.iter().try_fold(None, |sum, value| {
        let Some(value) = select(value) else {
            return Ok(sum);
        };
        sum.unwrap_or(0_i64)
            .checked_add(value)
            .map(Some)
            .ok_or(PersistenceError::InvalidRequestMetadataEvent)
    })
}

fn checked_optional_decimal_sum<T>(
    values: &[&T],
    select: impl Fn(&T) -> Option<Decimal>,
) -> Result<Option<Decimal>, PersistenceError> {
    values.iter().try_fold(None, |sum, value| {
        let Some(value) = select(value) else {
            return Ok(sum);
        };
        sum.unwrap_or(Decimal::ZERO)
            .checked_add(value)
            .map(Some)
            .ok_or(PersistenceError::InvalidRequestMetadataEvent)
    })
}
