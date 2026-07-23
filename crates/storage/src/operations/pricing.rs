use std::collections::HashSet;

use chrono::{DateTime, Utc};
use olp_domain::{OperationKind, ProviderKind};
use rust_decimal::Decimal;
use uuid::Uuid;

use super::{
    MAX_PAGE_SIZE,
    cursor::{OperationsError, OperationsPage},
};
use crate::{
    IdempotencyOutcome, IdempotencyResponse, PersistenceError, PgStore, ReplayableIdempotency,
    split_page,
    store::{
        ReplayableIdempotencyClaim, claim_replayable_idempotency, complete_replayable_idempotency,
    },
};

const PRICING_LOCK_ID: i64 = 0x4f4c_505f_5052; // "OLP_PR"

#[derive(Clone, Debug)]
pub struct PriceInput {
    pub provider_kind: ProviderKind,
    pub provider_id: Option<Uuid>,
    pub model: String,
    pub operation: OperationKind,
    pub input_per_million: Option<String>,
    pub output_per_million: Option<String>,
    pub unit_price: Option<String>,
    pub currency: String,
}

#[derive(Clone, Debug)]
pub struct PricingRevisionRecord {
    pub id: Uuid,
    pub revision: u32,
    pub effective_at: DateTime<Utc>,
    pub created_by: Uuid,
    pub created_at: DateTime<Utc>,
    pub prices: Vec<PriceInput>,
}

impl PgStore {
    pub async fn create_pricing_revision<F>(
        &self,
        actor: Uuid,
        idempotency_key: &str,
        effective_at: DateTime<Utc>,
        prices: &[PriceInput],
        replay: ReplayableIdempotency<'_>,
        build_response: F,
    ) -> Result<IdempotencyOutcome<PricingRevisionRecord>, OperationsError>
    where
        F: FnOnce(&PricingRevisionRecord) -> Result<IdempotencyResponse, PersistenceError>,
    {
        let mut transaction = self.pool().begin().await?;
        match claim_replayable_idempotency(
            &mut transaction,
            actor,
            "pricing_revision.create",
            idempotency_key,
            replay.request_fingerprint(),
            replay.master_key(),
        )
        .await?
        {
            ReplayableIdempotencyClaim::Execute => {}
            ReplayableIdempotencyClaim::Replay(response) => {
                transaction.rollback().await?;
                return Ok(IdempotencyOutcome::Replayed(response));
            }
            ReplayableIdempotencyClaim::Conflict => {
                transaction.rollback().await?;
                return Err(OperationsError::IdempotencyConflict);
            }
            ReplayableIdempotencyClaim::InProgress => {
                transaction.rollback().await?;
                return Err(OperationsError::IdempotencyInProgress);
            }
        }
        validate_prices(prices)?;
        sqlx::query!("SELECT pg_advisory_xact_lock($1)", PRICING_LOCK_ID)
            .fetch_one(&mut *transaction)
            .await?;
        let requested_currency = prices
            .first()
            .ok_or_else(|| OperationsError::Invalid("pricing revision is empty".to_owned()))?
            .currency
            .trim()
            .to_uppercase();
        let configured_currency: Option<String> = sqlx::query_scalar!(
            "SELECT currency::text AS \"value!\" FROM pricing_currency WHERE singleton"
        )
        .fetch_optional(&mut *transaction)
        .await?;
        if configured_currency
            .as_deref()
            .is_some_and(|currency| currency.trim() != requested_currency)
        {
            return Err(OperationsError::Invalid(format!(
                "pricing currency must match the installation currency {}",
                configured_currency
                    .as_deref()
                    .map(str::trim)
                    .unwrap_or_default()
            )));
        }
        let revision: i32 = sqlx::query_scalar!(
            "SELECT COALESCE(MAX(revision), 0) + 1 AS \"value!\" FROM pricing_revisions"
        )
        .fetch_one(&mut *transaction)
        .await?;
        let id = Uuid::now_v7();
        let now = Utc::now();
        sqlx::query!(
            "INSERT INTO pricing_revisions (id, revision, effective_at, created_by, created_at) \
             VALUES ($1, $2, $3, $4, $5)",
            id,
            revision,
            effective_at,
            actor,
            now
        )
        .execute(&mut *transaction)
        .await?;
        for price in prices {
            let input_per_million = parse_optional_decimal(price.input_per_million.as_deref())?;
            let output_per_million = parse_optional_decimal(price.output_per_million.as_deref())?;
            let unit_price = parse_optional_decimal(price.unit_price.as_deref())?;
            if let Some(provider_id) = price.provider_id {
                let provider_kind: Option<String> =
                    sqlx::query_scalar!("SELECT kind FROM providers WHERE id = $1", provider_id)
                        .fetch_optional(&mut *transaction)
                        .await?;
                if provider_kind.as_deref() != Some(price.provider_kind.as_str()) {
                    return Err(OperationsError::Invalid(
                        "a pricing override must reference a provider of the declared kind"
                            .to_owned(),
                    ));
                }
            }
            sqlx::query!(
                "INSERT INTO prices \
                 (pricing_revision_id, provider_kind, provider_id, model, operation, \
                  input_per_million, output_per_million, unit_price, currency) \
                 VALUES ($1, $2, $3, $4, $5, $6::numeric, $7::numeric, $8::numeric, $9)",
                id,
                price.provider_kind.as_str(),
                price.provider_id,
                price.model.trim(),
                price.operation.as_str(),
                input_per_million,
                output_per_million,
                unit_price,
                price.currency.trim().to_uppercase()
            )
            .execute(&mut *transaction)
            .await?;
        }
        sqlx::query!(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'pricing_revision.create', 'pricing_revision', $3, 'success', $4)",
            Uuid::now_v7(),
            actor,
            id.to_string(),
            now
        )
        .execute(&mut *transaction)
        .await?;
        let record = PricingRevisionRecord {
            id,
            revision: u32::try_from(revision)
                .map_err(|_| OperationsError::Invalid("revision overflow".to_owned()))?,
            effective_at,
            created_by: actor,
            created_at: now,
            prices: prices.to_vec(),
        };
        let response = build_response(&record)?;
        complete_replayable_idempotency(
            &mut transaction,
            actor,
            "pricing_revision.create",
            idempotency_key,
            replay.request_fingerprint(),
            replay.master_key(),
            &response,
        )
        .await?;
        transaction.commit().await?;
        Ok(IdempotencyOutcome::Executed {
            value: record,
            response,
        })
    }

    pub async fn pricing_revisions_page(
        &self,
        before_revision: Option<u32>,
        limit: u16,
    ) -> Result<OperationsPage<PricingRevisionRecord>, OperationsError> {
        let before_revision = before_revision
            .map(i32::try_from)
            .transpose()
            .map_err(|_| OperationsError::InvalidCursor)?;
        let page_size = limit.clamp(1, MAX_PAGE_SIZE);
        let rows = sqlx::query!(
            "SELECT r.id, r.revision, r.effective_at, r.created_by, r.created_at, \
                    p.provider_kind AS \"provider_kind?\", p.provider_id AS \"provider_id?\", \
                    p.model AS \"model?\", p.operation AS \"operation?\", \
                    p.input_per_million::text AS \"input_per_million?\", \
                    p.output_per_million::text AS \"output_per_million?\", \
                    p.unit_price::text AS \"unit_price?\", p.currency::text AS \"currency?\" \
             FROM pricing_revisions r LEFT JOIN prices p ON p.pricing_revision_id = r.id \
             WHERE r.id IN (SELECT id FROM pricing_revisions \
                            WHERE ($1::int IS NULL OR revision < $1) \
                            ORDER BY revision DESC LIMIT $2) \
             ORDER BY r.revision DESC, p.provider_kind, p.provider_id NULLS FIRST, \
                      p.model, p.operation",
            before_revision,
            i64::from(page_size) + 1
        )
        .fetch_all(self.pool())
        .await?;
        let mut revisions = Vec::<PricingRevisionRecord>::new();
        for row in rows {
            let id: Uuid = row.id;
            if revisions.last().is_none_or(|revision| revision.id != id) {
                revisions.push(PricingRevisionRecord {
                    id,
                    revision: u32::try_from(row.revision).map_err(|_| {
                        OperationsError::Invalid("stored pricing revision is invalid".to_owned())
                    })?,
                    effective_at: row.effective_at,
                    created_by: row.created_by,
                    created_at: row.created_at,
                    prices: Vec::new(),
                });
            }
            let provider_kind: Option<String> = row.provider_kind;
            if let Some(provider_kind) = provider_kind {
                let revision = revisions.last_mut().ok_or_else(|| {
                    OperationsError::Invalid("pricing revision grouping is invalid".to_owned())
                })?;
                revision.prices.push(PriceInput {
                    provider_kind: provider_kind.parse().map_err(|_| {
                        OperationsError::Invalid(
                            "stored pricing provider kind is invalid".to_owned(),
                        )
                    })?,
                    provider_id: row.provider_id,
                    model: row
                        .model
                        .ok_or(PersistenceError::InvalidStoredValue("pricing model"))?,
                    operation: row
                        .operation
                        .ok_or(PersistenceError::InvalidStoredValue("pricing operation"))?
                        .parse()
                        .map_err(|_| {
                            OperationsError::Invalid(
                                "stored pricing operation is invalid".to_owned(),
                            )
                        })?,
                    input_per_million: row.input_per_million,
                    output_per_million: row.output_per_million,
                    unit_price: row.unit_price,
                    currency: row
                        .currency
                        .ok_or(PersistenceError::InvalidStoredValue("pricing currency"))?
                        .trim()
                        .to_owned(),
                });
            }
        }
        let (revisions, next_cursor) = split_page(revisions, usize::from(page_size), |revision| {
            revision.revision.to_string()
        });
        Ok(OperationsPage {
            items: revisions,
            next_cursor,
        })
    }
}

pub(super) fn validate_prices(prices: &[PriceInput]) -> Result<(), OperationsError> {
    if prices.is_empty() || prices.len() > 10_000 {
        return Err(OperationsError::Invalid(
            "a pricing revision must contain 1-10000 entries".to_owned(),
        ));
    }
    let mut dimensions = HashSet::with_capacity(prices.len());
    let mut revision_currency: Option<String> = None;
    for price in prices {
        let currency = price.currency.trim();
        if price.model.trim().is_empty()
            || currency.len() != 3
            || !currency.bytes().all(|byte| byte.is_ascii_alphabetic())
            || (price.input_per_million.is_none()
                && price.output_per_million.is_none()
                && price.unit_price.is_none())
        {
            return Err(OperationsError::Invalid(
                "pricing entries require dimensions, ISO currency, and at least one price"
                    .to_owned(),
            ));
        }
        let normalized_currency = currency.to_ascii_uppercase();
        if revision_currency
            .as_ref()
            .is_some_and(|expected| expected != &normalized_currency)
        {
            return Err(OperationsError::Invalid(
                "a pricing revision cannot mix currencies".to_owned(),
            ));
        }
        revision_currency.get_or_insert(normalized_currency);
        if !dimensions.insert((
            price.provider_kind,
            price.provider_id,
            price.model.trim(),
            price.operation,
        )) {
            return Err(OperationsError::Invalid(
                "pricing revision contains duplicate scoped dimensions".to_owned(),
            ));
        }
        for amount in [
            &price.input_per_million,
            &price.output_per_million,
            &price.unit_price,
        ]
        .into_iter()
        .flatten()
        {
            validate_decimal(amount)?;
        }
    }
    Ok(())
}

pub(super) fn validate_decimal(value: &str) -> Result<(), OperationsError> {
    let value = value.trim();
    let mut parts = value.split('.');
    let integer = parts.next().unwrap_or_default();
    let fraction = parts.next();
    if value.is_empty()
        || value.starts_with('-')
        || parts.next().is_some()
        || integer.is_empty()
        || !integer.bytes().all(|byte| byte.is_ascii_digit())
        || fraction.is_some_and(|part| {
            part.is_empty() || part.len() > 12 || !part.bytes().all(|byte| byte.is_ascii_digit())
        })
        || integer.len() > 12
    {
        return Err(OperationsError::Invalid(
            "prices must be non-negative decimals with at most 12 fractional digits".to_owned(),
        ));
    }
    Ok(())
}

fn parse_optional_decimal(value: Option<&str>) -> Result<Option<Decimal>, OperationsError> {
    value
        .map(|value| {
            value.trim().parse().map_err(|_| {
                OperationsError::Invalid("price is outside the supported numeric range".to_owned())
            })
        })
        .transpose()
}
