use std::collections::BTreeSet;

use crate::access::policy::ApiKeyLimits;
use crate::access::policy::ApiKeyScope;
use crate::ids::RouteSlug;
use chrono::DateTime;
use chrono::Utc;
use thiserror::Error;
use uuid::Uuid;

use crate::access::audit_events::record_success;
use crate::crypto::key_material::ApiKey;
use crate::database::error::Error as PersistenceError;
use crate::database::idempotency::Outcome;
use crate::database::idempotency::Replayable;
use crate::database::idempotency::ReplayableIdempotencyClaim;
use crate::database::idempotency::Response;
use crate::database::idempotency::claim_idempotency;
use crate::database::idempotency::claim_replayable_idempotency;
use crate::database::idempotency::complete_idempotency;
use crate::database::idempotency::complete_replayable_idempotency;
use crate::runtime::publication::PublishedRuntimeRelease;
use crate::runtime::publication::compiler::RuntimeCompileError;
use crate::runtime::publication::compiler::compile_and_publish_runtime_in_transaction;
use crate::runtime::publication::compiler::prepare_runtime_mutation;

#[derive(Debug, Error)]
pub enum Error {
    #[error(transparent)]
    Persistence(#[from] PersistenceError),
    #[error(transparent)]
    RuntimeCompile(#[from] RuntimeCompileError),
    #[error("API key configuration is invalid: {0}")]
    Invalid(String),
    #[error("API key does not exist")]
    NotFound,
    #[error("API key ETag does not match")]
    PreconditionFailed,
    #[error("this idempotency key has already been used")]
    IdempotencyConflict,
    #[error("an operation with this idempotency key is still in progress")]
    IdempotencyInProgress,
}

impl From<sqlx::Error> for Error {
    fn from(error: sqlx::Error) -> Self {
        Self::Persistence(PersistenceError::Database(error))
    }
}

#[derive(Debug)]
pub struct NewApiKeyRecord {
    pub name: String,
    pub material: ApiKey,
    pub scopes: Vec<ApiKeyScope>,
    pub allowed_routes: Vec<RouteSlug>,
    pub limits: ApiKeyLimits,
    pub expires_at: Option<DateTime<Utc>>,
    pub actor: Uuid,
    pub idempotency_key: String,
}

#[derive(Debug, Clone)]
pub struct ApiKeyCreated {
    pub id: Uuid,
    pub lookup_id: String,
    pub etag: Uuid,
    pub release: PublishedRuntimeRelease,
}

#[derive(Debug, Clone)]
pub struct ApiKeyRevoked {
    pub etag: Uuid,
    pub release: PublishedRuntimeRelease,
}

fn validate_new_api_key_record(key: &NewApiKeyRecord) -> Result<(), Error> {
    if key.name.trim().is_empty() || key.name.chars().count() > 100 {
        return Err(Error::Invalid(
            "name must contain 1-100 characters".to_owned(),
        ));
    }
    if key.scopes.is_empty() {
        return Err(Error::Invalid("at least one scope is required".to_owned()));
    }
    if key.scopes.iter().copied().collect::<BTreeSet<_>>().len() != key.scopes.len() {
        return Err(Error::Invalid("scope entries must be unique".to_owned()));
    }
    if key
        .allowed_routes
        .iter()
        .cloned()
        .collect::<BTreeSet<_>>()
        .len()
        != key.allowed_routes.len()
    {
        return Err(Error::Invalid(
            "route allowlist entries must be unique".to_owned(),
        ));
    }
    if key
        .limits
        .daily_cost_limit
        .into_iter()
        .chain(key.limits.monthly_cost_limit)
        .any(|value| !crate::database::query::valid_cost_limit(value))
    {
        return Err(Error::Invalid(
            "cost limits must have at most 12 integer and 12 fractional digits".to_owned(),
        ));
    }
    Ok(())
}

fn validate_api_key_expiration(key: &NewApiKeyRecord) -> Result<(), Error> {
    if key
        .expires_at
        .is_some_and(|expiration| expiration <= Utc::now())
    {
        return Err(Error::Invalid(
            "expiration must be in the future".to_owned(),
        ));
    }
    Ok(())
}

enum ApiKeyCreateClaim<'a> {
    Execute(sqlx::Transaction<'a, sqlx::Postgres>),
    Replay(Response),
}

async fn claim_api_key_create<'a>(
    mut transaction: sqlx::Transaction<'a, sqlx::Postgres>,
    key: &NewApiKeyRecord,
    replay: Replayable<'_>,
) -> Result<ApiKeyCreateClaim<'a>, Error> {
    match claim_replayable_idempotency(
        &mut transaction,
        key.actor,
        "api_key.create",
        &key.idempotency_key,
        replay.request_fingerprint(),
        replay.master_key(),
    )
    .await?
    {
        ReplayableIdempotencyClaim::Execute => {
            prepare_runtime_mutation(&mut transaction).await?;
            Ok(ApiKeyCreateClaim::Execute(transaction))
        }
        ReplayableIdempotencyClaim::Replay(response) => {
            transaction.rollback().await?;
            Ok(ApiKeyCreateClaim::Replay(response))
        }
        ReplayableIdempotencyClaim::Conflict => {
            transaction.rollback().await?;
            Err(Error::IdempotencyConflict)
        }
        ReplayableIdempotencyClaim::InProgress => {
            transaction.rollback().await?;
            Err(Error::IdempotencyInProgress)
        }
    }
}

async fn insert_api_key_row(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    id: Uuid,
    etag: Uuid,
    key: &NewApiKeyRecord,
) -> Result<(), Error> {
    sqlx::query(
        "INSERT INTO api_keys \
         (id, lookup_id, secret_digest, name, created_by, expires_at, requests_per_minute, \
          tokens_per_minute, max_concurrency, daily_cost_limit, monthly_cost_limit, etag) \
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)",
    )
    .bind(id)
    .bind(&key.material.lookup_id)
    .bind(key.material.digest.to_vec())
    .bind(key.name.trim())
    .bind(key.actor)
    .bind(key.expires_at)
    .bind(
        key.limits
            .requests_per_minute
            .map(|value| i32::try_from(value.get()))
            .transpose()
            .map_err(|_| Error::Invalid("RPM limit is too large".to_owned()))?,
    )
    .bind(
        key.limits
            .tokens_per_minute
            .map(|value| i64::try_from(value.get()))
            .transpose()
            .map_err(|_| Error::Invalid("TPM limit is too large".to_owned()))?,
    )
    .bind(
        key.limits
            .concurrency
            .map(|value| i32::try_from(value.get()))
            .transpose()
            .map_err(|_| Error::Invalid("concurrency limit is too large".to_owned()))?,
    )
    .bind(key.limits.daily_cost_limit)
    .bind(key.limits.monthly_cost_limit)
    .bind(etag)
    .execute(&mut **transaction)
    .await?;
    let scopes = key
        .scopes
        .iter()
        .map(|scope| scope.as_str().to_owned())
        .collect::<Vec<_>>();
    sqlx::query("INSERT INTO api_key_scopes (api_key_id, scope) SELECT $1, UNNEST($2::text[])")
        .bind(id)
        .bind(&scopes)
        .execute(&mut **transaction)
        .await?;
    Ok(())
}

async fn insert_api_key_allowlist(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    id: Uuid,
    key: &NewApiKeyRecord,
) -> Result<(), Error> {
    let allowed_routes = key
        .allowed_routes
        .iter()
        .map(|route| route.as_str().to_owned())
        .collect::<Vec<_>>();
    let known: BTreeSet<String> =
        sqlx::query_scalar::<_, String>("SELECT slug FROM routes WHERE slug = ANY($1::text[])")
            .bind(&allowed_routes)
            .fetch_all(&mut **transaction)
            .await?
            .into_iter()
            .collect();
    if let Some(route) = key
        .allowed_routes
        .iter()
        .find(|route| !known.contains(route.as_str()))
    {
        return Err(Error::Invalid(format!(
            "allowlisted route {route} is not active"
        )));
    }
    sqlx::query(
        "INSERT INTO api_key_route_allowlist (api_key_id, route_slug) \
         SELECT $1, UNNEST($2::text[])",
    )
    .bind(id)
    .bind(&allowed_routes)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

pub async fn create_api_key_record<F>(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    key: &NewApiKeyRecord,
    replay: Replayable<'_>,
    build_response: F,
) -> Result<Outcome<ApiKeyCreated>, Error>
where
    F: FnOnce(&ApiKeyCreated) -> Result<Response, PersistenceError>,
{
    validate_new_api_key_record(key)?;
    let id = Uuid::now_v7();
    let etag = Uuid::now_v7();
    let transaction = pool
        .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
        .await?;
    let mut transaction = match claim_api_key_create(transaction, key, replay).await? {
        ApiKeyCreateClaim::Execute(transaction) => transaction,
        ApiKeyCreateClaim::Replay(response) => {
            return Ok(Outcome::Replayed(response));
        }
    };
    validate_api_key_expiration(key)?;
    insert_api_key_row(&mut transaction, id, etag, key).await?;
    insert_api_key_allowlist(&mut transaction, id, key).await?;
    record_success(
        &mut *transaction,
        provenance,
        key.actor,
        "api_key.create",
        "api_key",
        id,
    )
    .await?;
    let release = compile_and_publish_runtime_in_transaction(&mut transaction, key.actor).await?;
    let created = ApiKeyCreated {
        id,
        lookup_id: key.material.lookup_id.clone(),
        etag,
        release,
    };
    let response = build_response(&created)?;
    complete_replayable_idempotency(
        &mut transaction,
        key.actor,
        "api_key.create",
        &key.idempotency_key,
        replay.request_fingerprint(),
        replay.master_key(),
        &response,
    )
    .await?;
    transaction.commit().await?;
    Ok(Outcome::Executed {
        value: created,
        response,
    })
}

pub async fn revoke_api_key_record(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    id: Uuid,
    expected_etag: Uuid,
    actor: Uuid,
    idempotency_key: &str,
) -> Result<ApiKeyRevoked, Error> {
    let mut transaction = pool
        .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
        .await?;
    prepare_runtime_mutation(&mut transaction).await?;
    if !claim_idempotency(&mut transaction, actor, "api_key.revoke", idempotency_key).await? {
        return Err(Error::IdempotencyConflict);
    }
    let result = sqlx::query_as::<_, RevokeApiKeyRecordRow>(
        "UPDATE api_keys SET revoked_at = now(), etag = uuidv7() \
             WHERE id = $1 AND etag = $2 AND revoked_at IS NULL RETURNING etag",
    )
    .bind(id)
    .bind(expected_etag)
    .fetch_optional(&mut *transaction)
    .await?;
    let Some(result) = result else {
        let row =
            sqlx::query_as::<_, RevokeApiKeyRecordRow>("SELECT etag FROM api_keys WHERE id = $1")
                .bind(id)
                .fetch_optional(&mut *transaction)
                .await?;
        return Err(if row.is_some() {
            Error::PreconditionFailed
        } else {
            Error::NotFound
        });
    };
    complete_idempotency(
        &mut transaction,
        actor,
        "api_key.revoke",
        idempotency_key,
        &id.to_string(),
    )
    .await?;
    record_success(
        &mut *transaction,
        provenance,
        actor,
        "api_key.revoke",
        "api_key",
        id,
    )
    .await?;
    let release = compile_and_publish_runtime_in_transaction(&mut transaction, actor).await?;
    transaction.commit().await?;
    Ok(ApiKeyRevoked {
        etag: result.etag,
        release,
    })
}

#[derive(sqlx::FromRow)]
struct RevokeApiKeyRecordRow {
    etag: uuid::Uuid,
}
