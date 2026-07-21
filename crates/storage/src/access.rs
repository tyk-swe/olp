use std::collections::BTreeSet;

use chrono::{DateTime, Utc};
use olp_domain::{ApiKeyLimits, ApiKeyScope, RouteSlug};
use thiserror::Error;
use uuid::Uuid;

use crate::{
    ApiKeyMaterial, IdempotencyOutcome, IdempotencyResponse, PersistenceError, PgStore,
    PublishedRuntimeRelease, ReplayableIdempotency, RuntimeCompileError,
    runtime_compiler::{compile_and_publish_runtime_in_transaction, prepare_runtime_mutation},
    store::{
        ReplayableIdempotencyClaim, claim_idempotency, claim_replayable_idempotency,
        complete_idempotency, complete_replayable_idempotency,
    },
};

#[derive(Debug, Error)]
pub enum AccessError {
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

impl From<sqlx::Error> for AccessError {
    fn from(error: sqlx::Error) -> Self {
        Self::Persistence(PersistenceError::Database(error))
    }
}

#[derive(Debug)]
pub struct NewApiKeyRecord {
    pub name: String,
    pub material: ApiKeyMaterial,
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

impl PgStore {
    pub async fn create_api_key_record<F>(
        &self,
        key: &NewApiKeyRecord,
        replay: ReplayableIdempotency<'_>,
        build_response: F,
    ) -> Result<IdempotencyOutcome<ApiKeyCreated>, AccessError>
    where
        F: FnOnce(&ApiKeyCreated) -> Result<IdempotencyResponse, PersistenceError>,
    {
        if key.name.trim().is_empty() || key.name.chars().count() > 100 {
            return Err(AccessError::Invalid(
                "name must contain 1-100 characters".to_owned(),
            ));
        }
        if key.scopes.is_empty() {
            return Err(AccessError::Invalid(
                "at least one scope is required".to_owned(),
            ));
        }
        if key.scopes.iter().copied().collect::<BTreeSet<_>>().len() != key.scopes.len() {
            return Err(AccessError::Invalid(
                "scope entries must be unique".to_owned(),
            ));
        }
        if key
            .allowed_routes
            .iter()
            .cloned()
            .collect::<BTreeSet<_>>()
            .len()
            != key.allowed_routes.len()
        {
            return Err(AccessError::Invalid(
                "route allowlist entries must be unique".to_owned(),
            ));
        }
        let id = Uuid::now_v7();
        let etag = Uuid::now_v7();
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
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
            }
            ReplayableIdempotencyClaim::Replay(response) => {
                transaction.rollback().await?;
                return Ok(IdempotencyOutcome::Replayed(response));
            }
            ReplayableIdempotencyClaim::Conflict => {
                transaction.rollback().await?;
                return Err(AccessError::IdempotencyConflict);
            }
            ReplayableIdempotencyClaim::InProgress => {
                transaction.rollback().await?;
                return Err(AccessError::IdempotencyInProgress);
            }
        }
        if key
            .expires_at
            .is_some_and(|expiration| expiration <= Utc::now())
        {
            return Err(AccessError::Invalid(
                "expiration must be in the future".to_owned(),
            ));
        }
        sqlx::query(
            "INSERT INTO api_keys \
             (id, lookup_id, secret_digest, name, created_by, expires_at, requests_per_minute, \
              tokens_per_minute, max_concurrency, etag) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)",
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
                .map_err(|_| AccessError::Invalid("RPM limit is too large".to_owned()))?,
        )
        .bind(
            key.limits
                .tokens_per_minute
                .map(|value| i64::try_from(value.get()))
                .transpose()
                .map_err(|_| AccessError::Invalid("TPM limit is too large".to_owned()))?,
        )
        .bind(
            key.limits
                .concurrency
                .map(|value| i32::try_from(value.get()))
                .transpose()
                .map_err(|_| AccessError::Invalid("concurrency limit is too large".to_owned()))?,
        )
        .bind(etag)
        .execute(&mut *transaction)
        .await?;
        for scope in &key.scopes {
            sqlx::query("INSERT INTO api_key_scopes (api_key_id, scope) VALUES ($1, $2)")
                .bind(id)
                .bind(scope.as_str())
                .execute(&mut *transaction)
                .await?;
        }
        for route in &key.allowed_routes {
            let exists: bool =
                sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM routes WHERE slug = $1)")
                    .bind(route.as_str())
                    .fetch_one(&mut *transaction)
                    .await?;
            if !exists {
                return Err(AccessError::Invalid(format!(
                    "allowlisted route {route} is not active"
                )));
            }
            sqlx::query(
                "INSERT INTO api_key_route_allowlist (api_key_id, route_slug) VALUES ($1, $2)",
            )
            .bind(id)
            .bind(route.as_str())
            .execute(&mut *transaction)
            .await?;
        }
        sqlx::query(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome) \
             VALUES ($1, $2, 'api_key.create', 'api_key', $3, 'success')",
        )
        .bind(Uuid::now_v7())
        .bind(key.actor)
        .bind(id.to_string())
        .execute(&mut *transaction)
        .await?;
        let release =
            compile_and_publish_runtime_in_transaction(&mut transaction, key.actor).await?;
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
        Ok(IdempotencyOutcome::Executed {
            value: created,
            response,
        })
    }

    pub async fn revoke_api_key_record(
        &self,
        id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<ApiKeyRevoked, AccessError> {
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        prepare_runtime_mutation(&mut transaction).await?;
        if !claim_idempotency(&mut transaction, actor, "api_key.revoke", idempotency_key).await? {
            return Err(AccessError::IdempotencyConflict);
        }
        let result = sqlx::query(
            "UPDATE api_keys SET revoked_at = now(), etag = uuidv7() \
             WHERE id = $1 AND etag = $2 AND revoked_at IS NULL RETURNING etag",
        )
        .bind(id)
        .bind(expected_etag)
        .fetch_optional(&mut *transaction)
        .await?;
        let Some(result) = result else {
            let row = sqlx::query("SELECT etag FROM api_keys WHERE id = $1")
                .bind(id)
                .fetch_optional(&mut *transaction)
                .await?;
            return Err(if row.is_some() {
                AccessError::PreconditionFailed
            } else {
                AccessError::NotFound
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
        sqlx::query(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome) \
             VALUES ($1, $2, 'api_key.revoke', 'api_key', $3, 'success')",
        )
        .bind(Uuid::now_v7())
        .bind(actor)
        .bind(id.to_string())
        .execute(&mut *transaction)
        .await?;
        let release = compile_and_publish_runtime_in_transaction(&mut transaction, actor).await?;
        transaction.commit().await?;
        Ok(ApiKeyRevoked {
            etag: sqlx::Row::get(&result, "etag"),
            release,
        })
    }
}
