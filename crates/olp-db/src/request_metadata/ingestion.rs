mod attempt_facts;
use attempt_facts::insert_attempt_usage_fact;
use olp_engine::inference::request_metadata::{Event, RequestAttemptMetadata};
use rust_decimal::Decimal;
use sha2::{Digest, Sha256};
use uuid::Uuid;

use super::{
    REQUEST_METADATA_EVENT_FUTURE_SKEW_MINUTES, REQUEST_METADATA_EVENT_REPLAY_HORIZON_DAYS,
    validation::{ValidatedAttempt, ValidatedAttemptUsage, ValidatedRequestMetadata},
};
use crate::{error::Error, limits::costs::CostSnapshot, spend::add_cost_delta_on, store::Store};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Outcome {
    Persisted,
    Duplicate,
    RejectedOutsideReplayWindow,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct StreamPersistence {
    pub outcome: Outcome,
    pub cost_snapshot: Option<CostSnapshot>,
}

#[derive(Clone, Copy, Eq, PartialEq)]
enum AttemptChargeStatus {
    NotBillable,
    Billable,
    BillingUncertain,
}

enum ReceiptAdmission {
    Acquired,
    Duplicate,
    RejectedOutsideReplayWindow,
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

impl Store {
    /// Persists one idempotent metadata-only stream event. A bounded durable
    /// receipt protects the supported seven-day delivery window after raw
    /// facts roll into hourly usage. Older entries are rejected explicitly so
    /// they cannot silently add to an aggregate after their receipt expires.
    #[cfg(any(test, feature = "test-util"))]
    pub async fn persist_request_metadata_event(&self, event: &Event) -> Result<Outcome, Error> {
        let event_sha256: [u8; 32] = Sha256::digest(serde_json::to_vec(event)?).into();
        self.persist_request_metadata_event_with_digest(event, event_sha256)
            .await
            .map(|result| result.outcome)
    }

    /// Processes an event decoded from a Valkey Stream while fingerprinting
    /// the original bytes. Replays therefore remain stable across application
    /// versions even if Rust's serialization of [`Event`] later changes.
    pub async fn persist_request_metadata_stream_event(
        &self,
        event: &Event,
        original_payload: &[u8],
    ) -> Result<StreamPersistence, Error> {
        let event_sha256: [u8; 32] = Sha256::digest(original_payload).into();
        self.persist_request_metadata_event_with_digest(event, event_sha256)
            .await
    }

    async fn persist_request_metadata_event_with_digest(
        &self,
        event: &Event,
        event_sha256: [u8; 32],
    ) -> Result<StreamPersistence, Error> {
        let validated = ValidatedRequestMetadata::validate(event)?;
        let mut transaction = self.pool().begin().await?;
        match admit_request_metadata_receipt(&mut transaction, event, &event_sha256).await? {
            ReceiptAdmission::Acquired => {}
            ReceiptAdmission::Duplicate => {
                transaction.rollback().await?;
                return Ok(StreamPersistence {
                    outcome: Outcome::Duplicate,
                    cost_snapshot: None,
                });
            }
            ReceiptAdmission::RejectedOutsideReplayWindow => {
                transaction.commit().await?;
                return Ok(StreamPersistence {
                    outcome: Outcome::RejectedOutsideReplayWindow,
                    cost_snapshot: None,
                });
            }
        }
        insert_request_metadata_rows(&mut transaction, event, &validated).await?;

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
            return Ok(StreamPersistence {
                outcome: Outcome::Persisted,
                cost_snapshot: None,
            });
        }

        let persisted_facts =
            insert_attempt_usage_facts(&mut transaction, event, &validated.attempts).await?;
        let cost_snapshot = match cost_delta(&persisted_facts)? {
            Some((cost, unpriced_attempts)) => Some(
                add_cost_delta_on(
                    &mut transaction,
                    event.api_key_id,
                    event.observed_at,
                    cost,
                    unpriced_attempts,
                )
                .await?,
            ),
            None => None,
        };

        recompute_attempt_fact_markers(&mut transaction, event.request_id).await?;

        insert_compatibility_usage_fact_if_representable(&mut transaction, event, &persisted_facts)
            .await?;
        mark_request_metadata_receipt_persisted(&mut transaction, event.event_id, event.request_id)
            .await?;
        transaction.commit().await?;
        Ok(StreamPersistence {
            outcome: Outcome::Persisted,
            cost_snapshot,
        })
    }
}

fn cost_delta(facts: &[PersistedAttemptFact<'_>]) -> Result<Option<(Decimal, u64)>, Error> {
    if facts.is_empty() {
        return Ok(None);
    }
    let cost = facts.iter().try_fold(Decimal::ZERO, |sum, fact| {
        sum.checked_add(fact.estimated_cost.unwrap_or(Decimal::ZERO))
            .ok_or(Error::InvalidRequestMetadataEvent)
    })?;
    let unpriced_attempts = facts
        .iter()
        .filter(|fact| fact.charge_status != AttemptChargeStatus::NotBillable && fact.unpriced)
        .count()
        .try_into()
        .map_err(|_| Error::InvalidRequestMetadataEvent)?;
    Ok(Some((cost, unpriced_attempts)))
}

async fn insert_request_metadata_rows(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event: &Event,
    validated: &ValidatedRequestMetadata<'_>,
) -> Result<(), Error> {
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
    .execute(&mut **transaction)
    .await?;
    for attempt in &validated.attempts {
        sqlx::query!(
            "INSERT INTO attempts \
             (id, request_id, request_started_at, ordinal, provider_id, upstream_model, \
              started_at, completed_at, status_code, error_class, committed, latency_ms, first_byte_ms) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) \
             ON CONFLICT (request_id, ordinal) DO NOTHING",
            attempt.event.id,
            event.request_id,
            event.request_started_at,
            attempt.ordinal,
            attempt.event.provider_id,
            &attempt.event.upstream_model,
            attempt.event.started_at,
            attempt.event.completed_at,
            attempt.status_code,
            attempt.event.error_class.as_deref(),
            attempt.event.committed,
            attempt.latency_ms,
            attempt.first_byte_ms
        )
        .execute(&mut **transaction)
        .await?;
    }
    Ok(())
}

async fn insert_attempt_usage_facts<'a>(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event: &'a Event,
    attempts: &[ValidatedAttempt<'a>],
) -> Result<Vec<PersistedAttemptFact<'a>>, Error> {
    sqlx::query!(
        "INSERT INTO usage_request_anchors (request_id, request_started_at) \
         VALUES ($1, $2) ON CONFLICT DO NOTHING",
        event.request_id,
        event.request_started_at
    )
    .execute(&mut **transaction)
    .await?;

    let mut persisted_facts = Vec::with_capacity(attempts.len());
    for attempt in attempts {
        if let Some(fact) = insert_attempt_usage_fact(transaction, event, attempt).await? {
            persisted_facts.push(fact);
        }
    }
    Ok(persisted_facts)
}

async fn recompute_attempt_fact_markers(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    request_id: Uuid,
) -> Result<(), Error> {
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
                request_incomplete_counted = marked.request_marker AND marked.request_incomplete, \
                provider_incomplete_counted = marked.provider_marker AND marked.provider_incomplete, \
                model_incomplete_counted = marked.model_marker AND marked.model_incomplete, \
                target_incomplete_counted = marked.target_marker AND marked.target_incomplete \
           FROM marked WHERE fact.attempt_id = marked.attempt_id",
        request_id
    )
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

async fn admit_request_metadata_receipt(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event: &Event,
    event_sha256: &[u8; 32],
) -> Result<ReceiptAdmission, Error> {
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
    .fetch_optional(&mut **transaction)
    .await?;
    if receipt.is_some() {
        return Ok(ReceiptAdmission::Acquired);
    }

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
    .fetch_one(&mut **transaction)
    .await?;
    let exact_receipt = existing.receipt_exists
        && existing
            .event_sha256
            .is_none_or(|stored| stored.as_slice() == event_sha256.as_slice());
    if exact_receipt || existing.fact_exists || existing.attempt_fact_exists {
        return Ok(ReceiptAdmission::Duplicate);
    }
    if !existing.outside_window {
        return Err(Error::InvalidRequestMetadataEvent);
    }

    reject_expired_receipt(transaction, event, event_sha256).await
}

async fn mark_request_metadata_receipt_persisted(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event_id: Uuid,
    request_id: Uuid,
) -> Result<(), Error> {
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

async fn insert_compatibility_usage_fact_if_representable(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event: &Event,
    facts: &[PersistedAttemptFact<'_>],
) -> Result<(), Error> {
    // Keep a truthful request-level aggregate for older readers only when
    // every potentially billable attempt has the same provider/model.
    // Authoritative reads always use attempt_usage_facts.
    if let Some(first) = facts
        .iter()
        .find(|fact| fact.charge_status != AttemptChargeStatus::NotBillable)
        && facts
            .iter()
            .filter(|fact| fact.charge_status != AttemptChargeStatus::NotBillable)
            .all(|fact| {
                fact.attempt.provider_id == first.attempt.provider_id
                    && fact.attempt.upstream_model == first.attempt.upstream_model
            })
    {
        insert_compatibility_usage_fact(transaction, event, first, facts).await?;
    }
    Ok(())
}

async fn insert_compatibility_usage_fact(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event: &Event,
    attribution: &PersistedAttemptFact<'_>,
    facts: &[PersistedAttemptFact<'_>],
) -> Result<(), Error> {
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
) -> Result<Option<i64>, Error> {
    values.iter().try_fold(None, |sum, value| {
        let Some(value) = select(value) else {
            return Ok(sum);
        };
        sum.unwrap_or(0_i64)
            .checked_add(value)
            .map(Some)
            .ok_or(Error::InvalidRequestMetadataEvent)
    })
}

fn checked_optional_decimal_sum<T>(
    values: &[&T],
    select: impl Fn(&T) -> Option<Decimal>,
) -> Result<Option<Decimal>, Error> {
    values.iter().try_fold(None, |sum, value| {
        let Some(value) = select(value) else {
            return Ok(sum);
        };
        sum.unwrap_or(Decimal::ZERO)
            .checked_add(value)
            .map(Some)
            .ok_or(Error::InvalidRequestMetadataEvent)
    })
}

#[cfg(test)]
mod tests;

async fn reject_expired_receipt(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event: &Event,
    event_sha256: &[u8; 32],
) -> Result<ReceiptAdmission, Error> {
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
    .fetch_optional(&mut **transaction)
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
        .execute(&mut **transaction)
        .await?;
        return Ok(ReceiptAdmission::RejectedOutsideReplayWindow);
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
    .fetch_one(&mut **transaction)
    .await?;
    if exact_after_race {
        Ok(ReceiptAdmission::Duplicate)
    } else {
        Err(Error::InvalidRequestMetadataEvent)
    }
}
