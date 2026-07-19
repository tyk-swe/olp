use std::{
    collections::{BTreeMap, BTreeSet},
    num::{NonZeroU16, NonZeroU32, NonZeroU64},
};

use chrono::Utc;
use olp_domain::{
    ApiKey, ApiKeyDigest, ApiKeyId, ApiKeyLimits, ApiKeyLookupId, ApiKeyScope, ApiKeyStatus,
    Capability, CredentialVersionId, DurationMs, OperationKind, Provider, ProviderId, ProviderKind,
    Route, RouteId, RouteSlug, RuntimeGeneration, RuntimeGenerationId, RuntimeSnapshot, Surface,
    Target, TargetId, TransportMode,
};
use serde::Serialize;
use sha2::{Digest, Sha256};
use sqlx::{Postgres, Row, Transaction};
use thiserror::Error;
use uuid::Uuid;

use crate::{PersistenceError, PgStore, PublishedRelease};

const PUBLICATION_LOCK_ID: i64 = 0x4f4c_505f_5254; // "OLP_RT"

#[derive(Debug, Error)]
pub enum RuntimeCompileError {
    #[error(transparent)]
    Persistence(#[from] PersistenceError),
    #[error("stored runtime configuration is invalid: {0}")]
    InvalidConfiguration(String),
}

impl From<sqlx::Error> for RuntimeCompileError {
    fn from(error: sqlx::Error) -> Self {
        Self::Persistence(PersistenceError::Database(error))
    }
}

impl From<serde_json::Error> for RuntimeCompileError {
    fn from(error: serde_json::Error) -> Self {
        Self::Persistence(PersistenceError::Serialize(error))
    }
}

impl PgStore {
    /// Compiles normalized configuration while holding a cross-replica
    /// transaction lock, validates it, and publishes the release plus outbox
    /// hint atomically. Concurrent key/route activations therefore cannot
    /// publish an older view after a newer one.
    pub async fn compile_and_publish_runtime(
        &self,
        actor: Uuid,
    ) -> Result<PublishedRelease, RuntimeCompileError> {
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        prepare_runtime_mutation(&mut transaction).await?;
        let release = compile_and_publish_runtime_in_transaction(&mut transaction, actor).await?;
        transaction.commit().await?;
        Ok(release)
    }

    /// Loads a complete, transactionally consistent view of the API-key
    /// security state that is authoritative now. Fallback releases replace
    /// their historical key map with this view so an unchanged lookup ID can
    /// never regain an old digest, scope, allowlist, expiry, or hard limit.
    pub async fn current_runtime_api_keys(
        &self,
    ) -> Result<BTreeMap<ApiKeyLookupId, ApiKey>, RuntimeCompileError> {
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        prepare_runtime_mutation(&mut transaction).await?;
        let api_keys = compile_api_keys(&mut transaction).await?;
        transaction.commit().await?;
        Ok(api_keys)
    }
}

pub(crate) async fn prepare_runtime_mutation(
    transaction: &mut Transaction<'_, Postgres>,
) -> Result<(), RuntimeCompileError> {
    // Keep PostgreSQL's READ COMMITTED isolation. Each statement then observes
    // a fresh snapshot, including a concurrent winner that committed before
    // this transaction acquired the publication lock. Every active runtime
    // writer takes this lock, so the later mutation and compilation statements
    // remain mutually consistent. The runtime-generations trigger rejects
    // publications from older REPEATABLE READ writers during rolling upgrades.
    lock_runtime_publication(transaction).await
}

pub(crate) async fn lock_runtime_publication(
    transaction: &mut Transaction<'_, Postgres>,
) -> Result<(), RuntimeCompileError> {
    sqlx::query("SELECT pg_advisory_xact_lock($1)")
        .bind(PUBLICATION_LOCK_ID)
        .execute(&mut **transaction)
        .await?;
    Ok(())
}

pub(crate) async fn compile_and_publish_runtime_in_transaction(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
) -> Result<PublishedRelease, RuntimeCompileError> {
    let mut snapshot = compile_snapshot(transaction).await?;
    snapshot
        .validate()
        .map_err(|error| RuntimeCompileError::InvalidConfiguration(error.to_string()))?;
    let preliminary_payload = serde_json::to_vec(&snapshot)?;
    let preliminary_sha: [u8; 32] = Sha256::digest(&preliminary_payload).into();
    let generation_id = snapshot.generation.id.as_uuid();
    let now = Utc::now();
    let sequence: i64 = sqlx::query(
        "INSERT INTO runtime_generations \
         (id, compiled_release, release_sha256, created_by, created_at) \
         VALUES ($1, $2, $3, $4, $5) RETURNING sequence",
    )
    .bind(generation_id)
    .bind(&preliminary_payload)
    .bind(preliminary_sha.to_vec())
    .bind(actor)
    .bind(now)
    .fetch_one(&mut **transaction)
    .await?
    .get("sequence");
    sqlx::query(
        "INSERT INTO runtime_generation_provider_configs \
         (runtime_generation_id, provider_id, kind, endpoint, cloud_region, cloud_project, \
          deployment, api_version, auth_mode, active_credential_version_id, provider_revision_id) \
         SELECT $1, p.id, pr.kind, pr.endpoint, pr.cloud_region, pr.cloud_project, pr.deployment, \
                pr.api_version, pr.auth_mode, pr.credential_version_id, pr.id \
         FROM providers p JOIN provider_revisions pr ON pr.id = p.active_revision_id \
         WHERE p.state <> 'disabled'::provider_state",
    )
    .bind(generation_id)
    .execute(&mut **transaction)
    .await?;
    snapshot.generation.ordinal = u64::try_from(sequence).map_err(|_| {
        RuntimeCompileError::InvalidConfiguration(
            "runtime generation sequence is invalid".to_owned(),
        )
    })?;
    snapshot.generation.activated_at = now;
    let payload = serde_json::to_vec(&snapshot)?;
    let sha256: [u8; 32] = Sha256::digest(&payload).into();
    sqlx::query(
        "UPDATE runtime_generations SET compiled_release = $1, release_sha256 = $2 WHERE id = $3",
    )
    .bind(&payload)
    .bind(sha256.to_vec())
    .bind(generation_id)
    .execute(&mut **transaction)
    .await?;
    let hint = serde_json::to_vec(&RuntimeHint {
        generation_id,
        sequence,
    })?;
    sqlx::query(
        "INSERT INTO transactional_outbox \
         (id, topic, aggregate_id, payload, created_at) \
         VALUES ($1, 'runtime.generation.activated', $2, $3, $4)",
    )
    .bind(Uuid::now_v7())
    .bind(generation_id)
    .bind(hint)
    .bind(now)
    .execute(&mut **transaction)
    .await?;
    Ok(PublishedRelease {
        generation_id,
        sequence,
        payload,
        sha256,
        created_at: now,
    })
}

async fn compile_snapshot(
    transaction: &mut Transaction<'_, Postgres>,
) -> Result<RuntimeSnapshot, RuntimeCompileError> {
    let mut providers = BTreeMap::new();
    for row in sqlx::query(
        "SELECT p.id, pr.name, pr.kind, pr.credential_version_id \
         FROM providers p JOIN provider_revisions pr ON pr.id = p.active_revision_id \
         WHERE p.state <> 'disabled'::provider_state ORDER BY p.id",
    )
    .fetch_all(&mut **transaction)
    .await?
    {
        let id = ProviderId::from_uuid(row.get("id"));
        let credential: Option<Uuid> = row.get("credential_version_id");
        providers.insert(
            id,
            Provider {
                id,
                name: row.get("name"),
                kind: parse_provider_kind(row.get::<String, _>("kind").as_str())?,
                enabled: true,
                active_credential: credential.map(CredentialVersionId::from_uuid),
                capabilities: BTreeSet::new(),
            },
        );
    }
    for row in sqlx::query(
        "SELECT pr.provider_id, prm.upstream_model, prc.operation, prc.surface, prc.mode \
         FROM provider_revision_capabilities prc \
         JOIN provider_revision_models prm ON prm.id = prc.provider_revision_model_id \
         JOIN provider_revisions pr ON pr.id = prm.provider_revision_id \
         JOIN providers p ON p.id = pr.provider_id AND p.active_revision_id = pr.id \
         WHERE prm.enabled AND prc.source = 'certified' \
           AND p.state <> 'disabled'::provider_state \
         ORDER BY pr.provider_id, prm.upstream_model, prc.operation, prc.surface, prc.mode",
    )
    .fetch_all(&mut **transaction)
    .await?
    {
        let provider_id = ProviderId::from_uuid(row.get("provider_id"));
        let provider = providers.get_mut(&provider_id).ok_or_else(|| {
            RuntimeCompileError::InvalidConfiguration("capability has no active provider".into())
        })?;
        provider.capabilities.insert(Capability::new(
            row.get::<String, _>("upstream_model"),
            parse_operation(row.get::<String, _>("operation").as_str())?,
            parse_surface(row.get::<String, _>("surface").as_str())?,
            parse_mode(row.get::<String, _>("mode").as_str())?,
        ));
    }

    let route_rows = sqlx::query(
        "SELECT r.id, r.slug, rr.id AS revision_id, rr.routing_id, rr.overall_timeout_ms, rr.max_attempts \
         FROM routes r \
         JOIN LATERAL ( \
             SELECT id, routing_id, overall_timeout_ms, max_attempts FROM route_revisions \
             WHERE route_id = r.id ORDER BY revision DESC LIMIT 1 \
         ) rr ON true ORDER BY r.slug",
    )
    .fetch_all(&mut **transaction)
    .await?;
    let mut routes = BTreeMap::new();
    for row in route_rows {
        let revision_id: Uuid = row.get("revision_id");
        let slug = RouteSlug::parse(row.get::<String, _>("slug"))
            .map_err(|error| RuntimeCompileError::InvalidConfiguration(error.to_string()))?;
        let operations = sqlx::query(
            "SELECT operation FROM route_revision_operations \
             WHERE route_revision_id = $1 ORDER BY operation",
        )
        .bind(revision_id)
        .fetch_all(&mut **transaction)
        .await?
        .into_iter()
        .map(|row| parse_operation(row.get::<String, _>("operation").as_str()))
        .collect::<Result<BTreeSet<_>, _>>()?;
        let targets = sqlx::query(
            "SELECT rt.id, rt.routing_id, pr.provider_id, prm.upstream_model, rt.priority, rt.weight, rt.timeout_ms \
             FROM route_revision_targets rt \
             JOIN provider_revision_models prm \
               ON prm.source_provider_model_id = rt.provider_model_id \
             JOIN provider_revisions pr ON pr.id = prm.provider_revision_id \
             JOIN providers p ON p.id = pr.provider_id AND p.active_revision_id = pr.id \
             WHERE rt.route_revision_id = $1 AND prm.enabled \
               AND p.state <> 'disabled'::provider_state \
             ORDER BY rt.position",
        )
        .bind(revision_id)
        .fetch_all(&mut **transaction)
        .await?
        .into_iter()
        .map(|target| {
            let weight: i32 = target.get("weight");
            Ok(Target {
                id: TargetId::from_uuid(target.get("id")),
                routing_id: Some(TargetId::from_uuid(target.get("routing_id"))),
                provider_id: ProviderId::from_uuid(target.get("provider_id")),
                provider_model: target.get("upstream_model"),
                priority: u16::try_from(target.get::<i32, _>("priority")).map_err(|_| {
                    RuntimeCompileError::InvalidConfiguration("target priority is invalid".into())
                })?,
                weight: NonZeroU32::new(u32::try_from(weight).unwrap_or_default()).ok_or_else(
                    || RuntimeCompileError::InvalidConfiguration("target weight is zero".into()),
                )?,
                timeout: DurationMs::new(
                    u64::try_from(target.get::<i32, _>("timeout_ms")).map_err(|_| {
                        RuntimeCompileError::InvalidConfiguration("target timeout is invalid".into())
                    })?,
                ),
            })
        })
        .collect::<Result<Vec<_>, RuntimeCompileError>>()?;
        let overall_timeout_ms: i32 = row.get("overall_timeout_ms");
        let max_attempts: i16 = row.get("max_attempts");
        let route = Route {
            id: RouteId::from_uuid(row.get("id")),
            routing_id: Some(RouteId::from_uuid(row.get("routing_id"))),
            slug: slug.clone(),
            operations,
            overall_timeout: DurationMs::new(u64::try_from(overall_timeout_ms).map_err(|_| {
                RuntimeCompileError::InvalidConfiguration("route timeout is invalid".into())
            })?),
            max_attempts: NonZeroU16::new(u16::try_from(max_attempts).unwrap_or_default())
                .ok_or_else(|| {
                    RuntimeCompileError::InvalidConfiguration("route max attempts is zero".into())
                })?,
            targets,
        };
        routes.insert(slug, route);
    }

    let api_keys = compile_api_keys(transaction).await?;

    Ok(RuntimeSnapshot {
        generation: RuntimeGeneration {
            id: RuntimeGenerationId::new(),
            ordinal: 0,
            activated_at: Utc::now(),
        },
        providers,
        routes,
        api_keys,
    })
}

async fn compile_api_keys(
    transaction: &mut Transaction<'_, Postgres>,
) -> Result<BTreeMap<ApiKeyLookupId, ApiKey>, RuntimeCompileError> {
    let mut api_keys = BTreeMap::new();
    for row in sqlx::query(
        "SELECT id, lookup_id, secret_digest, expires_at, requests_per_minute, \
                tokens_per_minute, max_concurrency \
         FROM api_keys WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now()) \
         ORDER BY lookup_id",
    )
    .fetch_all(&mut **transaction)
    .await?
    {
        let id: Uuid = row.get("id");
        let lookup_id = ApiKeyLookupId::parse(row.get::<String, _>("lookup_id"))
            .map_err(|error| RuntimeCompileError::InvalidConfiguration(error.to_string()))?;
        let digest: Vec<u8> = row.get("secret_digest");
        let digest: [u8; 32] = digest.try_into().map_err(|_| {
            RuntimeCompileError::InvalidConfiguration("API key digest is not 32 bytes".into())
        })?;
        let scopes =
            sqlx::query("SELECT scope FROM api_key_scopes WHERE api_key_id = $1 ORDER BY scope")
                .bind(id)
                .fetch_all(&mut **transaction)
                .await?
                .into_iter()
                .map(|scope| match scope.get::<String, _>("scope").as_str() {
                    "inference" => Ok(ApiKeyScope::Inference),
                    "models_read" => Ok(ApiKeyScope::ModelsRead),
                    value => Err(RuntimeCompileError::InvalidConfiguration(format!(
                        "unknown API key scope {value}"
                    ))),
                })
                .collect::<Result<BTreeSet<_>, _>>()?;
        let allowed_routes = sqlx::query(
            "SELECT route_slug FROM api_key_route_allowlist WHERE api_key_id = $1 ORDER BY route_slug",
        )
        .bind(id)
        .fetch_all(&mut **transaction)
        .await?
        .into_iter()
        .map(|route| {
            RouteSlug::parse(route.get::<String, _>("route_slug")).map_err(|error| {
                RuntimeCompileError::InvalidConfiguration(error.to_string())
            })
        })
        .collect::<Result<BTreeSet<_>, _>>()?;
        let rpm: Option<i32> = row.get("requests_per_minute");
        let tpm: Option<i64> = row.get("tokens_per_minute");
        let concurrency: Option<i32> = row.get("max_concurrency");
        api_keys.insert(
            lookup_id.clone(),
            ApiKey {
                id: ApiKeyId::from_uuid(id),
                lookup_id,
                digest: ApiKeyDigest::new(digest),
                status: ApiKeyStatus::Active,
                expires_at: row.get("expires_at"),
                scopes,
                allowed_routes,
                limits: ApiKeyLimits {
                    requests_per_minute: optional_nonzero_u32(rpm, "requests_per_minute")?,
                    tokens_per_minute: optional_nonzero_u64(tpm, "tokens_per_minute")?,
                    concurrency: optional_nonzero_u32(concurrency, "max_concurrency")?,
                },
            },
        );
    }

    Ok(api_keys)
}

fn optional_nonzero_u32(
    value: Option<i32>,
    field: &str,
) -> Result<Option<NonZeroU32>, RuntimeCompileError> {
    value
        .map(|value| {
            u32::try_from(value)
                .ok()
                .and_then(NonZeroU32::new)
                .ok_or_else(|| {
                    RuntimeCompileError::InvalidConfiguration(format!(
                        "API key {field} limit is invalid"
                    ))
                })
        })
        .transpose()
}

fn optional_nonzero_u64(
    value: Option<i64>,
    field: &str,
) -> Result<Option<NonZeroU64>, RuntimeCompileError> {
    value
        .map(|value| {
            u64::try_from(value)
                .ok()
                .and_then(NonZeroU64::new)
                .ok_or_else(|| {
                    RuntimeCompileError::InvalidConfiguration(format!(
                        "API key {field} limit is invalid"
                    ))
                })
        })
        .transpose()
}

fn parse_provider_kind(value: &str) -> Result<ProviderKind, RuntimeCompileError> {
    value.parse().map_err(|_| {
        RuntimeCompileError::InvalidConfiguration(format!("unknown provider kind {value}"))
    })
}

fn parse_operation(value: &str) -> Result<OperationKind, RuntimeCompileError> {
    value.parse().map_err(|_| {
        RuntimeCompileError::InvalidConfiguration(format!("unknown operation {value}"))
    })
}

fn parse_surface(value: &str) -> Result<Surface, RuntimeCompileError> {
    value
        .parse()
        .map_err(|_| RuntimeCompileError::InvalidConfiguration(format!("unknown surface {value}")))
}

fn parse_mode(value: &str) -> Result<TransportMode, RuntimeCompileError> {
    value.parse().map_err(|_| {
        RuntimeCompileError::InvalidConfiguration(format!("unknown transport mode {value}"))
    })
}

#[derive(Serialize)]
struct RuntimeHint {
    generation_id: Uuid,
    sequence: i64,
}
