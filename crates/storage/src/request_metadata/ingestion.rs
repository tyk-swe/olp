use chrono::{DateTime, Utc};
use olp_domain::{OperationKind, Surface};
use rust_decimal::Decimal;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use sqlx::Row;
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
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RequestMetadataPersistenceOutcome {
    Persisted,
    Duplicate,
    RejectedOutsideReplayWindow,
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
        if event.request_completed_at < event.request_started_at
            || event.route_slug.trim().is_empty()
            || event
                .status_code
                .is_some_and(|status| !(100..=599).contains(&status))
            || !final_target_matches
            || !empty_attempt_metadata_is_valid
        {
            return Err(PersistenceError::InvalidRequestMetadataEvent);
        }
        let latency_ms = i32::try_from(event.latency_ms)
            .map_err(|_| PersistenceError::InvalidRequestMetadataEvent)?;
        let first_byte_ms = event
            .first_byte_ms
            .map(i32::try_from)
            .transpose()
            .map_err(|_| PersistenceError::InvalidRequestMetadataEvent)?;
        let status_code = event.status_code.map(i32::from);
        let attempt_count = i16::try_from(event.attempts.len())
            .map_err(|_| PersistenceError::InvalidRequestMetadataEvent)?;
        for (index, attempt) in event.attempts.iter().enumerate() {
            if usize::from(attempt.ordinal) != index + 1
                || attempt.completed_at < attempt.started_at
                || attempt
                    .status_code
                    .is_some_and(|status| !(100..=599).contains(&status))
                || i32::try_from(attempt.latency_ms).is_err()
                || attempt
                    .first_byte_ms
                    .is_some_and(|value| i32::try_from(value).is_err())
            {
                return Err(PersistenceError::InvalidRequestMetadataEvent);
            }
        }
        let mut transaction = self.pool().begin().await?;
        let receipt: Option<Uuid> = sqlx::query_scalar(
            "INSERT INTO request_metadata_event_receipts \
             (event_id, request_id, event_sha256, status, observed_at) \
             SELECT $1, $2, $3, 'pending'::request_metadata_event_receipt_status, $4 \
             WHERE $4 >= now() - make_interval(days => $5) \
               AND $4 <= now() + make_interval(mins => $6) \
               AND NOT EXISTS (SELECT 1 FROM usage_facts \
                               WHERE id = $1 OR request_id = $2) \
             ON CONFLICT DO NOTHING RETURNING event_id",
        )
        .bind(event.event_id)
        .bind(event.request_id)
        .bind(event_sha256.as_slice())
        .bind(event.observed_at)
        .bind(
            i32::try_from(REQUEST_METADATA_EVENT_REPLAY_HORIZON_DAYS)
                .expect("small replay horizon"),
        )
        .bind(i32::try_from(REQUEST_METADATA_EVENT_FUTURE_SKEW_MINUTES).expect("small future skew"))
        .fetch_optional(&mut *transaction)
        .await?;
        if receipt.is_none() {
            let existing = sqlx::query(
                "SELECT \
                   EXISTS (SELECT 1 FROM request_metadata_event_receipts \
                           WHERE event_id = $1 AND request_id = $2) AS receipt_exists, \
                   (SELECT event_sha256 FROM request_metadata_event_receipts \
                    WHERE event_id = $1 AND request_id = $2) AS event_sha256, \
                   EXISTS (SELECT 1 FROM usage_facts \
                           WHERE id = $1 AND request_id = $2) AS fact_exists, \
                   ($3 < now() - make_interval(days => $4) \
                    OR $3 > now() + make_interval(mins => $5)) AS outside_window",
            )
            .bind(event.event_id)
            .bind(event.request_id)
            .bind(event.observed_at)
            .bind(
                i32::try_from(REQUEST_METADATA_EVENT_REPLAY_HORIZON_DAYS)
                    .expect("small replay horizon"),
            )
            .bind(
                i32::try_from(REQUEST_METADATA_EVENT_FUTURE_SKEW_MINUTES)
                    .expect("small future skew"),
            )
            .fetch_one(&mut *transaction)
            .await?;
            let receipt_exists: bool = existing.get("receipt_exists");
            let exact_receipt = receipt_exists
                && existing
                    .get::<Option<Vec<u8>>, _>("event_sha256")
                    .is_none_or(|stored| stored.as_slice() == event_sha256);
            let exact_raw_fact: bool = existing.get("fact_exists");
            if exact_receipt || exact_raw_fact {
                transaction.rollback().await?;
                return Ok(RequestMetadataPersistenceOutcome::Duplicate);
            }
            if existing.get::<bool, _>("outside_window") {
                let rejection: Option<Uuid> = sqlx::query_scalar(
                    "INSERT INTO request_metadata_event_receipts \
                     (event_id, request_id, event_sha256, status, observed_at) \
                     SELECT $1, $2, $3, 'rejected'::request_metadata_event_receipt_status, $4 \
                     WHERE NOT EXISTS (SELECT 1 FROM usage_facts \
                                       WHERE id = $1 OR request_id = $2) \
                     ON CONFLICT DO NOTHING RETURNING event_id",
                )
                .bind(event.event_id)
                .bind(event.request_id)
                .bind(event_sha256.as_slice())
                .bind(event.observed_at)
                .fetch_optional(&mut *transaction)
                .await?;
                if rejection.is_some() {
                    sqlx::query(
                        "INSERT INTO request_metadata_ingestion_gaps \
                         (id, gateway_instance, event_count, reason, certainty, \
                          first_observed_at, last_observed_at) \
                         VALUES ($1, 'request-metadata-consumer', 0, \
                                 'request_metadata_event_outside_replay_window', \
                                 'lower_bound'::request_metadata_gap_certainty, now(), now())",
                    )
                    .bind(Uuid::now_v7())
                    .execute(&mut *transaction)
                    .await?;
                    transaction.commit().await?;
                    return Ok(RequestMetadataPersistenceOutcome::RejectedOutsideReplayWindow);
                }
                let exact_after_race: bool = sqlx::query_scalar(
                    "SELECT EXISTS ( \
                       SELECT 1 FROM request_metadata_event_receipts \
                       WHERE event_id = $1 AND request_id = $2 \
                         AND (event_sha256 IS NULL OR event_sha256 = $3) \
                       UNION ALL \
                       SELECT 1 FROM usage_facts WHERE id = $1 AND request_id = $2 \
                     )",
                )
                .bind(event.event_id)
                .bind(event.request_id)
                .bind(event_sha256.as_slice())
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
        sqlx::query(
            "INSERT INTO requests \
              (id, runtime_generation_id, api_key_id, route_slug, operation, surface, \
              started_at, completed_at, status_code, error_class, total_latency_ms, first_byte_ms, \
              attempt_count, created_at) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $8) \
             ON CONFLICT (id, started_at) DO NOTHING",
        )
        .bind(event.request_id)
        .bind(event.runtime_generation_id)
        .bind(event.api_key_id)
        .bind(&event.route_slug)
        .bind(event.operation.as_str())
        .bind(event.surface.as_str())
        .bind(event.request_started_at)
        .bind(event.request_completed_at)
        .bind(status_code)
        .bind(&event.error_class)
        .bind(latency_ms)
        .bind(first_byte_ms)
        .bind(attempt_count)
        .execute(&mut *transaction)
        .await?;
        for attempt in &event.attempts {
            let latency_ms = i32::try_from(attempt.latency_ms)
                .map_err(|_| PersistenceError::InvalidRequestMetadataEvent)?;
            let first_byte_ms = attempt
                .first_byte_ms
                .map(i32::try_from)
                .transpose()
                .map_err(|_| PersistenceError::InvalidRequestMetadataEvent)?;
            sqlx::query(
                "INSERT INTO attempts \
                 (id, request_id, request_started_at, ordinal, provider_id, upstream_model, \
                  started_at, completed_at, status_code, error_class, committed, latency_ms, first_byte_ms) \
                 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) \
                 ON CONFLICT (request_id, ordinal) DO NOTHING",
            )
            .bind(attempt.id)
            .bind(event.request_id)
            .bind(event.request_started_at)
            .bind(
                i16::try_from(attempt.ordinal)
                    .map_err(|_| PersistenceError::InvalidRequestMetadataEvent)?,
            )
            .bind(attempt.provider_id)
            .bind(&attempt.upstream_model)
            .bind(attempt.started_at)
            .bind(attempt.completed_at)
            .bind(attempt.status_code.map(i32::from))
            .bind(&attempt.error_class)
            .bind(attempt.committed)
            .bind(latency_ms)
            .bind(first_byte_ms)
            .execute(&mut *transaction)
            .await?;
        }

        // Authenticated decoding, route, and capability failures are valuable
        // operational metadata, but no provider usage exists to price or roll
        // up before the first attempt begins.
        if !has_attempts {
            transaction.commit().await?;
            return Ok(RequestMetadataPersistenceOutcome::Persisted);
        }

        let provider_id = event
            .provider_id
            .expect("validated attempted request metadata event has a provider ID");
        let upstream_model = event
            .upstream_model
            .as_deref()
            .expect("validated attempted request metadata event has an upstream model");
        sqlx::query(
            "INSERT INTO usage_request_anchors (request_id, request_started_at) \
             VALUES ($1, $2) ON CONFLICT DO NOTHING",
        )
        .bind(event.request_id)
        .bind(event.request_started_at)
        .execute(&mut *transaction)
        .await?;
        let pricing = sqlx::query(
            "SELECT selected.pricing_revision_id, selected.currency, \
                    selected.pricing_revision_id IS NOT NULL \
                      AND ($5::bigint IS NULL OR selected.input_per_million IS NOT NULL) \
                      AND ($6::bigint IS NULL OR selected.output_per_million IS NOT NULL) \
                      AND ($7::numeric IS NULL OR selected.unit_price IS NOT NULL) \
                      AS pricing_complete, \
                    CASE WHEN $8::boolean \
                               AND selected.pricing_revision_id IS NOT NULL \
                               AND ($5::bigint IS NULL OR selected.input_per_million IS NOT NULL) \
                               AND ($6::bigint IS NULL OR selected.output_per_million IS NOT NULL) \
                               AND ($7::numeric IS NULL OR selected.unit_price IS NOT NULL) \
                         THEN (COALESCE($5::numeric * selected.input_per_million / 1000000, 0) \
                             + COALESCE($6::numeric * selected.output_per_million / 1000000, 0) \
                             + COALESCE($7::numeric * selected.unit_price, 0))::text \
                         ELSE NULL END AS estimated_cost \
             FROM providers provider \
             LEFT JOIN LATERAL ( \
                 SELECT revision.id AS pricing_revision_id, price.input_per_million, \
                        price.output_per_million, price.unit_price, price.currency::text AS currency \
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
        )
        .bind(provider_id)
        .bind(upstream_model)
        .bind(event.operation.as_str())
        .bind(event.observed_at)
        .bind(event.input_tokens)
        .bind(event.output_tokens)
        .bind(event.media_units)
        .bind(event.usage_complete)
        .fetch_one(&mut *transaction)
        .await?;
        let pricing_revision_id: Option<Uuid> = pricing.get("pricing_revision_id");
        let pricing_complete: bool = pricing.get("pricing_complete");
        let estimated_cost: Option<String> = pricing.get("estimated_cost");
        let currency = pricing
            .get::<Option<String>, _>("currency")
            .map(|value| value.trim().to_owned());
        let unpriced = !pricing_complete;
        sqlx::query(
            "INSERT INTO usage_facts \
             (id, request_id, request_started_at, api_key_id, provider_id, route_slug, upstream_model, operation, \
              surface, observed_at, input_tokens, output_tokens, cached_input_tokens, media_units, \
              estimated_cost, unpriced, usage_complete, pricing_revision_id, currency) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, \
                     $15::numeric, $16, $17, $18, $19) \
             ON CONFLICT (request_id) DO NOTHING",
        )
        .bind(event.event_id)
        .bind(event.request_id)
        .bind(event.request_started_at)
        .bind(event.api_key_id)
        .bind(provider_id)
        .bind(&event.route_slug)
        .bind(upstream_model)
        .bind(event.operation.as_str())
        .bind(event.surface.as_str())
        .bind(event.observed_at)
        .bind(event.input_tokens)
        .bind(event.output_tokens)
        .bind(event.cached_input_tokens)
        .bind(event.media_units)
        .bind(estimated_cost)
        .bind(unpriced)
        .bind(event.usage_complete)
        .bind(pricing_revision_id)
        .bind(currency)
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        Ok(RequestMetadataPersistenceOutcome::Persisted)
    }
}
