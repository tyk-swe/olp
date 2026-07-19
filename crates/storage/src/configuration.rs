use chrono::{DateTime, Utc};
use olp_domain::{
    OperationKind, ProviderAuthMode, ProviderId, ProviderKind, RouteSlug, RuntimeSnapshot, Surface,
};
use sqlx::{Postgres, Row, Transaction};
use thiserror::Error;
use uuid::Uuid;

use crate::{
    EncryptedSecret, IdempotencyOutcome, IdempotencyResponse, PersistenceError, PgStore,
    PublishedRelease, ReplayableIdempotency, RuntimeCompileError,
    runtime_compiler::{
        compile_and_publish_runtime_in_transaction, lock_runtime_publication,
        prepare_runtime_mutation,
    },
    store::{
        ReplayableIdempotencyClaim, claim_idempotency, claim_replayable_idempotency,
        complete_idempotency, complete_replayable_idempotency,
    },
};

#[derive(Debug, Error)]
pub enum ConfigurationError {
    #[error(transparent)]
    Persistence(#[from] PersistenceError),
    #[error("stored encrypted credential is malformed")]
    InvalidCredential,
    #[error("provider does not exist")]
    ProviderNotFound,
    #[error("provider cannot be activated without a credential and enabled model")]
    ProviderIncomplete,
    #[error("provider ETag does not match")]
    PreconditionFailed,
    #[error("route draft does not exist")]
    RouteNotFound,
    #[error("route draft is not validated")]
    RouteNotValidated,
    #[error("route draft is invalid: {0}")]
    InvalidRoute(String),
    #[error(transparent)]
    RuntimeCompile(#[from] RuntimeCompileError),
    #[error("this idempotency key has already been used")]
    IdempotencyConflict,
    #[error("an operation with this idempotency key is still in progress")]
    IdempotencyInProgress,
}

impl From<sqlx::Error> for ConfigurationError {
    fn from(error: sqlx::Error) -> Self {
        Self::Persistence(PersistenceError::Database(error))
    }
}

#[derive(Debug)]
pub struct NewProviderDraft {
    pub provider_id: Uuid,
    pub credential_id: Option<Uuid>,
    pub model_id: Option<Uuid>,
    pub name: String,
    pub kind: ProviderKind,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    pub auth_mode: ProviderAuthMode,
    pub connector_ready: bool,
    pub credential: Option<EncryptedSecret>,
    pub model: Option<String>,
    pub display_name: Option<String>,
    pub model_enabled: bool,
    pub surface: Option<Surface>,
    pub actor: Uuid,
    pub idempotency_key: String,
}

#[derive(Debug, Clone)]
pub struct ProviderDraftCreated {
    pub provider_id: Uuid,
    pub credential_id: Option<Uuid>,
    pub model_id: Option<Uuid>,
    pub etag: Uuid,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Clone)]
pub struct ProviderActivated {
    pub etag: Uuid,
    pub release: PublishedRelease,
}

#[derive(Debug, Clone)]
pub struct ProviderSecretRecord {
    pub provider_id: ProviderId,
    pub kind: ProviderKind,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    pub auth_mode: ProviderAuthMode,
    pub credential_id: Option<Uuid>,
    pub credential_version: Option<u32>,
    pub encrypted: Option<EncryptedSecret>,
}

#[derive(Debug, Clone)]
pub struct NewRouteTarget {
    pub provider_id: Uuid,
    pub provider_model: String,
    pub priority: u16,
    pub weight: u32,
    pub timeout_ms: u64,
}

#[derive(Debug)]
pub struct NewRouteDraft {
    pub slug: String,
    pub operations: Vec<OperationKind>,
    pub overall_timeout_ms: u64,
    pub max_attempts: u16,
    pub targets: Vec<NewRouteTarget>,
    pub actor: Uuid,
    pub idempotency_key: String,
}

#[derive(Debug, Clone)]
pub struct RouteDraftCreated {
    pub id: Uuid,
    pub slug: RouteSlug,
    pub etag: Uuid,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Clone)]
pub struct RouteActivated {
    pub route_id: Uuid,
    pub revision_id: Uuid,
    pub revision: i32,
    pub release: PublishedRelease,
}

impl PgStore {
    /// Loads the release-exact connector configuration and credential named by
    /// a verified runtime sidecar. Mutable catalog drafts are deliberately not
    /// consulted, so testing a replacement endpoint or credential cannot alter
    /// the transport used by the last activated provider revision.
    pub async fn provider_secrets_for_runtime(
        &self,
        snapshot: &RuntimeSnapshot,
    ) -> Result<Vec<ProviderSecretRecord>, ConfigurationError> {
        let mut records = Vec::with_capacity(snapshot.providers.len());
        for runtime_provider in snapshot.providers.values() {
            let expected_credential = runtime_provider
                .active_credential
                .map(|credential| credential.as_uuid());
            let row = sqlx::query(
                "SELECT rpc.provider_id AS id, rpc.kind, rpc.endpoint, rpc.cloud_region, \
                        rpc.cloud_project, rpc.deployment, rpc.api_version, rpc.auth_mode, \
                        cv.id AS credential_id, cv.version AS credential_version, \
                        cv.ciphertext, cv.nonce, cv.master_key_version \
                 FROM runtime_generation_provider_configs rpc \
                 JOIN providers p ON p.id = rpc.provider_id \
                 LEFT JOIN provider_credential_versions cv \
                   ON cv.id = rpc.active_credential_version_id AND cv.revoked_at IS NULL \
                 WHERE rpc.provider_id = $1 AND rpc.runtime_generation_id = $3 \
                   AND rpc.active_credential_version_id IS NOT DISTINCT FROM $2 \
                   AND p.active_revision_id IS NOT NULL \
                   AND p.state <> 'disabled'::provider_state \
                   AND (rpc.active_credential_version_id IS NULL OR cv.id IS NOT NULL)",
            )
            .bind(runtime_provider.id.as_uuid())
            .bind(expected_credential)
            .bind(snapshot.generation.id.as_uuid())
            .fetch_optional(self.pool())
            .await?
            .ok_or(ConfigurationError::InvalidCredential)?;
            let stored_kind = parse_provider_kind(row.get::<String, _>("kind").as_str())?;
            if stored_kind != runtime_provider.kind {
                return Err(ConfigurationError::InvalidCredential);
            }
            records.push(provider_secret_from_row(row)?);
        }
        Ok(records)
    }

    pub async fn create_provider_draft<F>(
        &self,
        provider: NewProviderDraft,
        replay: ReplayableIdempotency<'_>,
        build_response: F,
    ) -> Result<IdempotencyOutcome<ProviderDraftCreated>, ConfigurationError>
    where
        F: FnOnce(&ProviderDraftCreated) -> Result<IdempotencyResponse, PersistenceError>,
    {
        let mut transaction = self.pool().begin().await?;
        match claim_replayable_idempotency(
            &mut transaction,
            provider.actor,
            "provider.create_draft",
            &provider.idempotency_key,
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
                return Err(ConfigurationError::IdempotencyConflict);
            }
            ReplayableIdempotencyClaim::InProgress => {
                transaction.rollback().await?;
                return Err(ConfigurationError::IdempotencyInProgress);
            }
        }
        if provider.credential.is_some() != provider.credential_id.is_some() {
            return Err(ConfigurationError::InvalidCredential);
        }
        if provider.model.is_some() != provider.model_id.is_some()
            || provider.model.is_some() != provider.display_name.is_some()
            || (provider.model.is_none() && (provider.model_enabled || provider.surface.is_some()))
        {
            return Err(ConfigurationError::ProviderIncomplete);
        }
        let now = Utc::now();
        let etag = Uuid::now_v7();
        sqlx::query(
            "INSERT INTO providers \
             (id, name, kind, state, endpoint, cloud_region, cloud_project, deployment, \
              api_version, auth_mode, connector_ready, etag, created_by, created_at, updated_at) \
             VALUES ($1, $2, $3, 'draft'::provider_state, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13)",
        )
        .bind(provider.provider_id)
        .bind(provider.name.trim())
        .bind(provider.kind.as_str())
        .bind(provider.endpoint.as_deref())
        .bind(provider.cloud_region.as_deref())
        .bind(provider.cloud_project.as_deref())
        .bind(provider.deployment.as_deref())
        .bind(provider.api_version.as_deref())
        .bind(provider.auth_mode.as_str())
        .bind(provider.connector_ready)
        .bind(etag)
        .bind(provider.actor)
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        if let (Some(credential_id), Some(credential)) =
            (provider.credential_id, provider.credential.as_ref())
        {
            let master_key_version = database_version(credential.key_version)?;
            sqlx::query(
                "INSERT INTO provider_credential_versions \
                 (id, provider_id, version, ciphertext, nonce, master_key_version, created_by, created_at) \
                 VALUES ($1, $2, 1, $3, $4, $5, $6, $7)",
            )
            .bind(credential_id)
            .bind(provider.provider_id)
            .bind(&credential.ciphertext)
            .bind(credential.nonce.to_vec())
            .bind(master_key_version)
            .bind(provider.actor)
            .bind(now)
            .execute(&mut *transaction)
            .await?;
            sqlx::query("UPDATE providers SET active_credential_version_id = $1 WHERE id = $2")
                .bind(credential_id)
                .bind(provider.provider_id)
                .execute(&mut *transaction)
                .await?;
        }
        if let (Some(model_id), Some(model), Some(display_name)) =
            (provider.model_id, &provider.model, &provider.display_name)
        {
            sqlx::query(
                "INSERT INTO provider_models \
                 (id, provider_id, upstream_model, display_name, enabled, discovered_at, created_at, \
                  inventory_source, availability, first_seen_at, last_seen_at) \
                 VALUES ($1, $2, $3, $4, $5, $6, $6, 'configured', 'available', $6, $6)",
            )
            .bind(model_id)
            .bind(provider.provider_id)
            .bind(model.trim())
            .bind(display_name.trim())
            .bind(provider.model_enabled)
            .bind(now)
            .execute(&mut *transaction)
            .await?;
        }
        if let (Some(surface), Some(model_id)) = (&provider.surface, provider.model_id) {
            for mode in ["unary", "streaming"] {
                sqlx::query(
                    "INSERT INTO model_capabilities \
                     (provider_model_id, operation, surface, mode, source, certified_at) \
                     VALUES ($1, 'generation', $2, $3, 'declared', NULL)",
                )
                .bind(model_id)
                .bind(surface.as_str())
                .bind(mode)
                .execute(&mut *transaction)
                .await?;
            }
        }
        sqlx::query(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'provider.create_draft', 'provider', $3, 'success', $4)",
        )
        .bind(Uuid::now_v7())
        .bind(provider.actor)
        .bind(provider.provider_id.to_string())
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        let created = ProviderDraftCreated {
            provider_id: provider.provider_id,
            credential_id: provider.credential_id,
            model_id: provider.model_id,
            etag,
            created_at: now,
        };
        let response = build_response(&created)?;
        complete_replayable_idempotency(
            &mut transaction,
            provider.actor,
            "provider.create_draft",
            &provider.idempotency_key,
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

    pub async fn activate_provider(
        &self,
        provider_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<ProviderActivated, ConfigurationError> {
        let new_etag = Uuid::now_v7();
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        prepare_runtime_mutation(&mut transaction).await?;
        if !claim_idempotency(
            &mut transaction,
            actor,
            "provider.activate",
            idempotency_key,
        )
        .await?
        {
            return Err(ConfigurationError::IdempotencyConflict);
        }
        let provider = sqlx::query(
            "SELECT p.name, p.kind, p.state::text AS state, p.endpoint, p.cloud_region, \
                    p.cloud_project, p.deployment, p.api_version, p.auth_mode, \
                    p.connector_ready, p.etag, p.active_credential_version_id, \
                    ar.credential_version_id AS previously_activated_credential_id, \
                    (p.last_probe_status = 'succeeded' AND p.last_probe_at IS NOT NULL \
                     AND p.last_probe_context_id = p.certification_context_id) AS probe_ready, \
                    ((p.auth_mode IN ('adc', 'default_chain') \
                      AND p.active_credential_version_id IS NULL) OR EXISTS ( \
                         SELECT 1 FROM provider_credential_versions cv \
                         WHERE cv.id = p.active_credential_version_id \
                           AND cv.provider_id = p.id AND cv.revoked_at IS NULL)) AS credential_ready, \
                    EXISTS (SELECT 1 FROM provider_models pm \
                            WHERE pm.provider_id = p.id AND pm.enabled) AS has_model, \
                    NOT EXISTS ( \
                      SELECT 1 FROM provider_models pm \
                       WHERE pm.provider_id = p.id AND pm.enabled AND ( \
                         pm.availability <> 'available' OR \
                         NOT EXISTS (SELECT 1 FROM model_capabilities mc \
                                     WHERE mc.provider_model_id = pm.id) OR \
                         EXISTS (SELECT 1 FROM model_capabilities mc \
                                 WHERE mc.provider_model_id = pm.id \
                                   AND (mc.source <> 'certified' \
                                     OR mc.certification_context_id IS DISTINCT FROM p.certification_context_id \
                                     OR mc.review_revision IS DISTINCT FROM pm.review_revision)))) \
                       AS capabilities_ready \
             FROM providers p \
             LEFT JOIN provider_revisions ar ON ar.id = p.active_revision_id \
             WHERE p.id = $1 FOR UPDATE OF p",
        )
        .bind(provider_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(ConfigurationError::ProviderNotFound)?;
        if provider.get::<Uuid, _>("etag") != expected_etag {
            return Err(ConfigurationError::PreconditionFailed);
        }
        if provider.get::<String, _>("state") != "draft"
            || !provider.get::<bool, _>("connector_ready")
            || !provider.get::<bool, _>("probe_ready")
            || !provider.get::<bool, _>("credential_ready")
            || !provider.get::<bool, _>("has_model")
            || !provider.get::<bool, _>("capabilities_ready")
        {
            return Err(ConfigurationError::ProviderIncomplete);
        }

        // Media reservations are short RowExclusive transactions. Holding a
        // table SHARE lock makes this activation decision atomic with respect
        // to new upstream jobs on every gateway replica.
        sqlx::query("LOCK TABLE async_media_jobs IN SHARE MODE")
            .execute(&mut *transaction)
            .await?;

        let revision: i32 = sqlx::query_scalar(
            "SELECT COALESCE(max(revision), 0) + 1 FROM provider_revisions WHERE provider_id = $1",
        )
        .bind(provider_id)
        .fetch_one(&mut *transaction)
        .await?;
        let revision_id = Uuid::now_v7();
        sqlx::query(
            "INSERT INTO provider_revisions \
             (id, provider_id, revision, name, kind, endpoint, cloud_region, cloud_project, \
              deployment, api_version, auth_mode, connector_ready, credential_version_id, \
              source_etag, activated_by) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)",
        )
        .bind(revision_id)
        .bind(provider_id)
        .bind(revision)
        .bind(provider.get::<String, _>("name"))
        .bind(provider.get::<String, _>("kind"))
        .bind(provider.get::<Option<String>, _>("endpoint"))
        .bind(provider.get::<Option<String>, _>("cloud_region"))
        .bind(provider.get::<Option<String>, _>("cloud_project"))
        .bind(provider.get::<Option<String>, _>("deployment"))
        .bind(provider.get::<Option<String>, _>("api_version"))
        .bind(provider.get::<String, _>("auth_mode"))
        .bind(provider.get::<bool, _>("connector_ready"))
        .bind(provider.get::<Option<Uuid>, _>("active_credential_version_id"))
        .bind(expected_etag)
        .bind(actor)
        .execute(&mut *transaction)
        .await?;
        sqlx::query(
            "INSERT INTO provider_revision_models \
             (id, provider_revision_id, source_provider_model_id, upstream_model, \
              display_name, enabled, discovered_at) \
             SELECT uuidv7(), $1, pm.id, pm.upstream_model, pm.display_name, pm.enabled, \
                    pm.discovered_at FROM provider_models pm WHERE pm.provider_id = $2",
        )
        .bind(revision_id)
        .bind(provider_id)
        .execute(&mut *transaction)
        .await?;
        sqlx::query(
            "INSERT INTO provider_revision_capabilities \
              (provider_revision_model_id, operation, surface, mode, source, certified_at, \
               certification_context_id, certification_run_id, certification_evidence_kind) \
              SELECT prm.id, mc.operation, mc.surface, mc.mode, mc.source, mc.certified_at, \
                     mc.certification_context_id, mc.certification_run_id, mc.certification_evidence_kind \
             FROM provider_revision_models prm \
             JOIN model_capabilities mc ON mc.provider_model_id = prm.source_provider_model_id \
             WHERE prm.provider_revision_id = $1",
        )
        .bind(revision_id)
        .execute(&mut *transaction)
        .await?;
        let incompatible_media_job: Option<Uuid> = sqlx::query_scalar(
            "SELECT j.id
             FROM async_media_jobs j
             JOIN providers p ON p.id = j.provider_id
             LEFT JOIN provider_revisions authority
               ON authority.id = COALESCE(j.provider_revision_id, p.active_revision_id)
             JOIN provider_revisions candidate ON candidate.id = $2
             WHERE j.provider_id = $1 AND j.lifecycle_state <> 'deleted'
               AND (
                 authority.id IS NULL
                 OR authority.kind IS DISTINCT FROM candidate.kind
                 OR authority.endpoint IS DISTINCT FROM candidate.endpoint
                 OR authority.cloud_region IS DISTINCT FROM candidate.cloud_region
                 OR authority.cloud_project IS DISTINCT FROM candidate.cloud_project
                 OR authority.deployment IS DISTINCT FROM candidate.deployment
                 OR authority.api_version IS DISTINCT FROM candidate.api_version
                 OR authority.auth_mode IS DISTINCT FROM candidate.auth_mode
                 OR authority.credential_version_id IS DISTINCT FROM candidate.credential_version_id
                 OR NOT EXISTS (
                   SELECT 1 FROM provider_revision_models prm
                   WHERE prm.provider_revision_id = candidate.id
                     AND prm.upstream_model = j.provider_model AND prm.enabled
                     AND NOT EXISTS (
                       SELECT required.operation
                       FROM (VALUES ('video_get'), ('video_content'), ('video_delete'))
                            AS required(operation)
                       WHERE NOT EXISTS (
                         SELECT 1 FROM provider_revision_capabilities prc
                         WHERE prc.provider_revision_model_id = prm.id
                           AND prc.operation = required.operation
                           AND prc.surface = CASE j.surface
                               WHEN 'openai' THEN 'open_ai' ELSE j.surface END
                           AND prc.mode = 'unary' AND prc.source = 'certified'
                       )
                     )
                 )
               )
             ORDER BY j.created_at, j.id LIMIT 1",
        )
        .bind(provider_id)
        .bind(revision_id)
        .fetch_optional(&mut *transaction)
        .await?;
        if incompatible_media_job.is_some() {
            return Err(ConfigurationError::ProviderIncomplete);
        }
        let uncovered_route_operation: Option<String> = sqlx::query_scalar(
            "SELECT concat(r.slug, '/', rro.operation) \
             FROM routes r \
             JOIN LATERAL (SELECT id FROM route_revisions \
                           WHERE route_id = r.id ORDER BY revision DESC LIMIT 1) rr ON true \
             JOIN route_revision_operations rro ON rro.route_revision_id = rr.id \
             WHERE NOT EXISTS ( \
               SELECT 1 FROM route_revision_targets rt \
               JOIN provider_models pm ON pm.id = rt.provider_model_id \
               JOIN providers target_provider ON target_provider.id = pm.provider_id \
               JOIN provider_revision_models prm \
                 ON prm.source_provider_model_id = pm.id \
                AND prm.provider_revision_id = CASE WHEN target_provider.id = $1 \
                                                    THEN $2 \
                                                    ELSE target_provider.active_revision_id END \
                AND prm.enabled \
               JOIN provider_revision_capabilities prc \
                 ON prc.provider_revision_model_id = prm.id \
                AND prc.operation = rro.operation AND prc.source = 'certified' \
               WHERE rt.route_revision_id = rr.id \
                 AND target_provider.state <> 'disabled'::provider_state) \
             ORDER BY r.slug, rro.operation LIMIT 1",
        )
        .bind(provider_id)
        .bind(revision_id)
        .fetch_optional(&mut *transaction)
        .await?;
        if uncovered_route_operation.is_some() {
            return Err(ConfigurationError::ProviderIncomplete);
        }
        sqlx::query(
            "UPDATE providers SET state = 'active'::provider_state, active_revision_id = $1, \
                    etag = $2, updated_at = now() WHERE id = $3 AND etag = $4",
        )
        .bind(revision_id)
        .bind(new_etag)
        .bind(provider_id)
        .bind(expected_etag)
        .execute(&mut *transaction)
        .await?;

        let previous_credential: Option<Uuid> = provider.get("previously_activated_credential_id");
        let activated_credential: Option<Uuid> = provider.get("active_credential_version_id");
        if previous_credential.is_some() && previous_credential != activated_credential {
            sqlx::query(
                "UPDATE provider_credential_versions SET revoked_at = COALESCE(revoked_at, now()) \
                 WHERE id = $1 AND provider_id = $2",
            )
            .bind(previous_credential)
            .bind(provider_id)
            .execute(&mut *transaction)
            .await?;
        }
        sqlx::query(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome) \
             VALUES ($1, $2, 'provider.activate', 'provider', $3, 'success')",
        )
        .bind(Uuid::now_v7())
        .bind(actor)
        .bind(provider_id.to_string())
        .execute(&mut *transaction)
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "provider.activate",
            idempotency_key,
            &provider_id.to_string(),
        )
        .await?;
        let release = compile_and_publish_runtime_in_transaction(&mut transaction, actor).await?;
        transaction.commit().await?;
        Ok(ProviderActivated {
            etag: new_etag,
            release,
        })
    }

    pub async fn create_route_draft<F>(
        &self,
        route: NewRouteDraft,
        replay: ReplayableIdempotency<'_>,
        build_response: F,
    ) -> Result<IdempotencyOutcome<RouteDraftCreated>, ConfigurationError>
    where
        F: FnOnce(&RouteDraftCreated) -> Result<IdempotencyResponse, PersistenceError>,
    {
        let mut transaction = self.pool().begin().await?;
        match claim_replayable_idempotency(
            &mut transaction,
            route.actor,
            "route.create_draft",
            &route.idempotency_key,
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
                return Err(ConfigurationError::IdempotencyConflict);
            }
            ReplayableIdempotencyClaim::InProgress => {
                transaction.rollback().await?;
                return Err(ConfigurationError::IdempotencyInProgress);
            }
        }
        let slug = RouteSlug::parse(route.slug)
            .map_err(|error| ConfigurationError::InvalidRoute(error.to_string()))?;
        if route.operations.is_empty() {
            return Err(ConfigurationError::InvalidRoute(
                "at least one operation is required".to_owned(),
            ));
        }
        if route.targets.is_empty() {
            return Err(ConfigurationError::InvalidRoute(
                "at least one target is required".to_owned(),
            ));
        }
        if route.max_attempts == 0 || usize::from(route.max_attempts) > route.targets.len() {
            return Err(ConfigurationError::InvalidRoute(
                "max_attempts must be between one and the target count".to_owned(),
            ));
        }
        let overall_timeout_ms = i32::try_from(route.overall_timeout_ms).map_err(|_| {
            ConfigurationError::InvalidRoute("overall timeout is too large".to_owned())
        })?;
        if overall_timeout_ms <= 0 {
            return Err(ConfigurationError::InvalidRoute(
                "overall timeout must be positive".to_owned(),
            ));
        }
        let id = Uuid::now_v7();
        let routing_id = Uuid::now_v7();
        let etag = Uuid::now_v7();
        let now = Utc::now();
        sqlx::query(
            "INSERT INTO route_drafts \
             (id, routing_id, slug, state, overall_timeout_ms, max_attempts, etag, created_by, created_at, updated_at) \
             VALUES ($1, $2, $3, 'draft'::route_draft_state, $4, $5, $6, $7, $8, $8)",
        )
        .bind(id)
        .bind(routing_id)
        .bind(slug.as_str())
        .bind(overall_timeout_ms)
        .bind(i16::try_from(route.max_attempts).map_err(|_| {
            ConfigurationError::InvalidRoute("max attempts is too large".to_owned())
        })?)
        .bind(etag)
        .bind(route.actor)
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        for operation in route.operations {
            sqlx::query(
                "INSERT INTO route_draft_operations (route_draft_id, operation) VALUES ($1, $2)",
            )
            .bind(id)
            .bind(operation.as_str())
            .execute(&mut *transaction)
            .await?;
        }
        for (position, target) in route.targets.into_iter().enumerate() {
            if target.weight == 0
                || target.timeout_ms == 0
                || target.timeout_ms > route.overall_timeout_ms
            {
                return Err(ConfigurationError::InvalidRoute(
                    "target weight/timeout is invalid".to_owned(),
                ));
            }
            let provider_model_id: Option<Uuid> = sqlx::query_scalar(
                "SELECT prm.source_provider_model_id \
                 FROM providers p \
                 JOIN provider_revision_models prm ON prm.provider_revision_id = p.active_revision_id \
                 WHERE p.id = $1 AND prm.upstream_model = $2 AND prm.enabled \
                   AND p.state <> 'disabled'::provider_state",
            )
            .bind(target.provider_id)
            .bind(target.provider_model.trim())
            .fetch_optional(&mut *transaction)
            .await?;
            let provider_model_id = provider_model_id.ok_or_else(|| {
                ConfigurationError::InvalidRoute(format!(
                    "target provider/model {}/{} is not active",
                    target.provider_id, target.provider_model
                ))
            })?;
            sqlx::query(
                "INSERT INTO route_draft_targets \
                 (id, routing_id, route_draft_id, provider_model_id, priority, weight, timeout_ms, position) \
                 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
            )
            .bind(Uuid::now_v7())
            .bind(Uuid::now_v7())
            .bind(id)
            .bind(provider_model_id)
            .bind(i32::from(target.priority))
            .bind(i32::try_from(target.weight).map_err(|_| {
                ConfigurationError::InvalidRoute("target weight is too large".to_owned())
            })?)
            .bind(i32::try_from(target.timeout_ms).map_err(|_| {
                ConfigurationError::InvalidRoute("target timeout is too large".to_owned())
            })?)
            .bind(
                i32::try_from(position)
                    .map_err(|_| ConfigurationError::InvalidRoute("too many targets".to_owned()))?,
            )
            .execute(&mut *transaction)
            .await?;
        }
        sqlx::query(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'route.create_draft', 'route_draft', $3, 'success', $4)",
        )
        .bind(Uuid::now_v7())
        .bind(route.actor)
        .bind(id.to_string())
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        let created = RouteDraftCreated {
            id,
            slug,
            etag,
            created_at: now,
        };
        let response = build_response(&created)?;
        complete_replayable_idempotency(
            &mut transaction,
            route.actor,
            "route.create_draft",
            &route.idempotency_key,
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

    pub async fn validate_route_draft(
        &self,
        draft_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
    ) -> Result<(Uuid, RouteSlug), ConfigurationError> {
        let mut transaction = self.pool().begin().await?;
        let row = sqlx::query("SELECT etag, slug FROM route_drafts WHERE id = $1")
            .bind(draft_id)
            .fetch_optional(&mut *transaction)
            .await?;
        let Some(row) = row else {
            return Err(ConfigurationError::RouteNotFound);
        };
        if row.get::<Uuid, _>("etag") != expected_etag {
            return Err(ConfigurationError::PreconditionFailed);
        }
        revalidate_route_draft(&mut transaction, draft_id).await?;
        let etag = Uuid::now_v7();
        let updated = sqlx::query(
            "UPDATE route_drafts SET state = 'validated'::route_draft_state, etag = $1, updated_at = now() \
             WHERE id = $2 AND etag = $3",
        )
        .bind(etag)
        .bind(draft_id)
        .bind(expected_etag)
        .execute(&mut *transaction)
        .await?;
        if updated.rows_affected() != 1 {
            return Err(ConfigurationError::PreconditionFailed);
        }
        let slug = RouteSlug::parse(row.get::<String, _>("slug"))
            .map_err(|error| ConfigurationError::InvalidRoute(error.to_string()))?;
        sqlx::query(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome) \
             VALUES ($1, $2, 'route.validate_draft', 'route_draft', $3, 'success')",
        )
        .bind(Uuid::now_v7())
        .bind(actor)
        .bind(draft_id.to_string())
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        Ok((etag, slug))
    }

    pub async fn activate_route_draft(
        &self,
        draft_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<RouteActivated, ConfigurationError> {
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        // READ COMMITTED is intentional here. Acquiring an advisory lock is a
        // statement in PostgreSQL, so a REPEATABLE READ transaction would pin
        // its snapshot before waiting for a concurrent provider activation.
        // With the publication lock held, READ COMMITTED observes the exact
        // active revisions that won the lock immediately before this draft.
        lock_runtime_publication(&mut transaction).await?;
        if !claim_idempotency(&mut transaction, actor, "route.activate", idempotency_key).await? {
            return Err(ConfigurationError::IdempotencyConflict);
        }
        sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))")
            .bind(draft_id.to_string())
            .execute(&mut *transaction)
            .await?;
        let draft = sqlx::query(
            "SELECT rd.slug, rd.routing_id, rd.state::text AS state, rd.etag, rd.overall_timeout_ms, \
                    rd.max_attempts, rr.route_id AS based_route_id, rr.slug AS based_slug \
             FROM route_drafts rd \
             LEFT JOIN route_revisions rr ON rr.id = rd.based_on_revision_id \
             WHERE rd.id = $1 FOR UPDATE OF rd",
        )
        .bind(draft_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(ConfigurationError::RouteNotFound)?;
        if draft.get::<Uuid, _>("etag") != expected_etag {
            return Err(ConfigurationError::PreconditionFailed);
        }
        if draft.get::<String, _>("state") != "validated" {
            return Err(ConfigurationError::RouteNotValidated);
        }
        // Media reservations are not runtime-publication mutations. Block
        // their short INSERT/UPDATE transactions while checking and
        // publishing so a job cannot appear against the old route after the
        // compatibility decision but before this activation commits.
        sqlx::query("LOCK TABLE async_media_jobs IN SHARE MODE")
            .execute(&mut *transaction)
            .await?;
        revalidate_route_draft(&mut transaction, draft_id).await?;
        let slug: String = draft.get("slug");
        let based_route_id: Option<Uuid> = draft.get("based_route_id");
        let based_slug: Option<String> = draft.get("based_slug");
        let route_id = if let Some(route_id) = based_route_id {
            if based_slug.as_deref() != Some(slug.as_str()) {
                return Err(ConfigurationError::InvalidRoute(
                    "a restored route draft must retain its original stable slug".to_owned(),
                ));
            }
            route_id
        } else {
            sqlx::query(
                "INSERT INTO routes (id, slug, created_by) VALUES ($1, $2, $3) \
                 ON CONFLICT (slug) DO UPDATE SET slug = EXCLUDED.slug RETURNING id",
            )
            .bind(Uuid::now_v7())
            .bind(&slug)
            .bind(actor)
            .fetch_one(&mut *transaction)
            .await?
            .get("id")
        };
        let revision: i32 = sqlx::query_scalar(
            "SELECT COALESCE(max(revision), 0) + 1 FROM route_revisions WHERE route_id = $1",
        )
        .bind(route_id)
        .fetch_one(&mut *transaction)
        .await?;
        let revision_id = Uuid::now_v7();
        sqlx::query(
            "INSERT INTO route_revisions \
             (id, route_id, routing_id, revision, slug, overall_timeout_ms, max_attempts, source_draft_id, activated_by) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)",
        )
        .bind(revision_id)
        .bind(route_id)
        .bind(draft.get::<Uuid, _>("routing_id"))
        .bind(revision)
        .bind(&slug)
        .bind(draft.get::<i32, _>("overall_timeout_ms"))
        .bind(draft.get::<i16, _>("max_attempts"))
        .bind(draft_id)
        .bind(actor)
        .execute(&mut *transaction)
        .await?;
        sqlx::query(
            "INSERT INTO route_revision_operations (route_revision_id, operation) \
             SELECT $1, operation FROM route_draft_operations WHERE route_draft_id = $2",
        )
        .bind(revision_id)
        .bind(draft_id)
        .execute(&mut *transaction)
        .await?;
        let targets = sqlx::query(
            "SELECT routing_id, provider_model_id, priority, weight, timeout_ms, position \
             FROM route_draft_targets WHERE route_draft_id = $1 ORDER BY position",
        )
        .bind(draft_id)
        .fetch_all(&mut *transaction)
        .await?;
        for target in targets {
            sqlx::query(
                "INSERT INTO route_revision_targets \
                 (id, routing_id, route_revision_id, provider_model_id, priority, weight, timeout_ms, position) \
                 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
            )
            .bind(Uuid::now_v7())
            .bind(target.get::<Uuid, _>("routing_id"))
            .bind(revision_id)
            .bind(target.get::<Uuid, _>("provider_model_id"))
            .bind(target.get::<i32, _>("priority"))
            .bind(target.get::<i32, _>("weight"))
            .bind(target.get::<i32, _>("timeout_ms"))
            .bind(target.get::<i32, _>("position"))
            .execute(&mut *transaction)
            .await?;
        }
        sqlx::query(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome) \
             VALUES ($1, $2, 'route.activate', 'route', $3, 'success')",
        )
        .bind(Uuid::now_v7())
        .bind(actor)
        .bind(route_id.to_string())
        .execute(&mut *transaction)
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "route.activate",
            idempotency_key,
            &route_id.to_string(),
        )
        .await?;
        let release = compile_and_publish_runtime_in_transaction(&mut transaction, actor).await?;
        transaction.commit().await?;
        Ok(RouteActivated {
            route_id,
            revision_id,
            revision,
            release,
        })
    }
}

async fn revalidate_route_draft(
    transaction: &mut Transaction<'_, Postgres>,
    draft_id: Uuid,
) -> Result<(), ConfigurationError> {
    let target_count: i64 = sqlx::query_scalar(
        "SELECT count(*)::bigint FROM route_draft_targets WHERE route_draft_id = $1",
    )
    .bind(draft_id)
    .fetch_one(&mut **transaction)
    .await?;
    if target_count == 0 {
        return Err(ConfigurationError::InvalidRoute(
            "the draft must contain at least one target".to_owned(),
        ));
    }

    let unavailable: Option<String> = sqlx::query_scalar(
        "SELECT concat(p.name, '/', pm.upstream_model) \
         FROM route_draft_targets rdt \
         JOIN provider_models pm ON pm.id = rdt.provider_model_id \
         JOIN providers p ON p.id = pm.provider_id \
         LEFT JOIN provider_revision_models prm \
           ON prm.provider_revision_id = p.active_revision_id \
          AND prm.source_provider_model_id = pm.id \
         WHERE rdt.route_draft_id = $1 \
           AND (p.state = 'disabled'::provider_state OR prm.id IS NULL OR NOT prm.enabled) \
         ORDER BY rdt.position LIMIT 1",
    )
    .bind(draft_id)
    .fetch_optional(&mut **transaction)
    .await?;
    if let Some(unavailable) = unavailable {
        return Err(ConfigurationError::InvalidRoute(format!(
            "target provider/model is not in an activated provider revision: {unavailable}"
        )));
    }

    let uncovered_operation: Option<String> = sqlx::query_scalar(
        "SELECT rdo.operation FROM route_draft_operations rdo \
         WHERE rdo.route_draft_id = $1 AND NOT EXISTS ( \
           SELECT 1 FROM route_draft_targets rdt \
           JOIN provider_models pm ON pm.id = rdt.provider_model_id \
           JOIN providers p ON p.id = pm.provider_id \
           JOIN provider_revision_models prm \
             ON prm.provider_revision_id = p.active_revision_id \
            AND prm.source_provider_model_id = pm.id AND prm.enabled \
           JOIN provider_revision_capabilities prc \
             ON prc.provider_revision_model_id = prm.id \
            AND prc.operation = rdo.operation AND prc.source = 'certified' \
           WHERE rdt.route_draft_id = $1 \
             AND p.state <> 'disabled'::provider_state) \
         ORDER BY rdo.operation LIMIT 1",
    )
    .bind(draft_id)
    .fetch_optional(&mut **transaction)
    .await?;
    if let Some(operation) = uncovered_operation {
        return Err(ConfigurationError::InvalidRoute(format!(
            "no activated target has a certified capability for route operation {operation}"
        )));
    }

    let media_job_without_target: Option<Uuid> = sqlx::query_scalar(
        "SELECT j.id FROM async_media_jobs j \
         JOIN route_drafts rd ON rd.id = $1 AND rd.slug = j.route_slug \
         WHERE j.lifecycle_state <> 'deleted' AND NOT EXISTS ( \
           SELECT 1 FROM route_draft_targets rdt \
           JOIN provider_models pm ON pm.id = rdt.provider_model_id \
           WHERE rdt.route_draft_id = rd.id \
             AND pm.provider_id = j.provider_id \
             AND pm.upstream_model = j.provider_model) \
         ORDER BY j.created_at, j.id LIMIT 1",
    )
    .bind(draft_id)
    .fetch_optional(&mut **transaction)
    .await?;
    if let Some(job_id) = media_job_without_target {
        return Err(ConfigurationError::InvalidRoute(format!(
            "active media job {job_id} requires its exact provider/model target"
        )));
    }

    let media_job_without_lifecycle: Option<String> = sqlx::query_scalar(
        "SELECT concat(j.id::text, '/', required.operation) \
         FROM async_media_jobs j \
         JOIN route_drafts rd ON rd.id = $1 AND rd.slug = j.route_slug \
         CROSS JOIN (VALUES ('video_get'), ('video_content'), ('video_delete')) \
                    AS required(operation) \
         WHERE j.lifecycle_state <> 'deleted' AND ( \
           NOT EXISTS ( \
             SELECT 1 FROM route_draft_operations rdo \
             WHERE rdo.route_draft_id = rd.id AND rdo.operation = required.operation) \
           OR NOT EXISTS ( \
             SELECT 1 FROM route_draft_targets rdt \
             JOIN provider_models pm ON pm.id = rdt.provider_model_id \
             JOIN providers p ON p.id = pm.provider_id \
             JOIN provider_revision_models prm \
               ON prm.provider_revision_id = p.active_revision_id \
              AND prm.source_provider_model_id = pm.id AND prm.enabled \
             JOIN provider_revision_capabilities prc \
               ON prc.provider_revision_model_id = prm.id \
              AND prc.operation = required.operation \
              AND prc.surface = CASE j.surface WHEN 'openai' THEN 'open_ai' ELSE j.surface END \
              AND prc.mode = 'unary' AND prc.source = 'certified' \
             WHERE rdt.route_draft_id = rd.id \
               AND p.state <> 'disabled'::provider_state \
               AND pm.provider_id = j.provider_id \
               AND pm.upstream_model = j.provider_model)) \
         ORDER BY j.created_at, j.id, required.operation LIMIT 1",
    )
    .bind(draft_id)
    .fetch_optional(&mut **transaction)
    .await?;
    if let Some(requirement) = media_job_without_lifecycle {
        return Err(ConfigurationError::InvalidRoute(format!(
            "active media job requires an exact certified lifecycle capability: {requirement}"
        )));
    }

    Ok(())
}

fn provider_secret_from_row(
    row: sqlx::postgres::PgRow,
) -> Result<ProviderSecretRecord, ConfigurationError> {
    let credential_id: Option<Uuid> = row.get("credential_id");
    let credential_version = row
        .get::<Option<i32>, _>("credential_version")
        .map(stored_version)
        .transpose()?;
    let nonce = row.get::<Option<Vec<u8>>, _>("nonce");
    let ciphertext = row.get::<Option<Vec<u8>>, _>("ciphertext");
    let key_version = row
        .get::<Option<i32>, _>("master_key_version")
        .map(stored_version)
        .transpose()?;
    let encrypted = match (nonce, ciphertext, key_version) {
        (Some(nonce), Some(ciphertext), Some(key_version)) => Some(EncryptedSecret {
            key_version,
            nonce: nonce
                .try_into()
                .map_err(|_| ConfigurationError::InvalidCredential)?,
            ciphertext,
        }),
        (None, None, None) => None,
        _ => return Err(ConfigurationError::InvalidCredential),
    };
    if credential_id.is_some() != credential_version.is_some()
        || credential_id.is_some() != encrypted.is_some()
    {
        return Err(ConfigurationError::InvalidCredential);
    }
    Ok(ProviderSecretRecord {
        provider_id: ProviderId::from_uuid(row.get("id")),
        kind: parse_provider_kind(row.get::<String, _>("kind").as_str())?,
        endpoint: row.get("endpoint"),
        cloud_region: row.get("cloud_region"),
        cloud_project: row.get("cloud_project"),
        deployment: row.get("deployment"),
        api_version: row.get("api_version"),
        auth_mode: row.get::<String, _>("auth_mode").parse().map_err(|_| {
            PersistenceError::InvalidStoredValue("runtime provider authentication mode")
        })?,
        credential_id,
        credential_version,
        encrypted,
    })
}

fn parse_provider_kind(value: &str) -> Result<ProviderKind, ConfigurationError> {
    value
        .parse()
        .map_err(|_| ConfigurationError::InvalidCredential)
}

fn database_version(version: u32) -> Result<i32, ConfigurationError> {
    i32::try_from(version)
        .ok()
        .filter(|version| *version > 0)
        .ok_or(ConfigurationError::InvalidCredential)
}

fn stored_version(version: i32) -> Result<u32, ConfigurationError> {
    u32::try_from(version)
        .ok()
        .filter(|version| *version > 0)
        .ok_or(ConfigurationError::InvalidCredential)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn credential_versions_must_be_positive_database_integers() {
        assert_eq!(database_version(1).unwrap(), 1);
        assert_eq!(database_version(i32::MAX as u32).unwrap(), i32::MAX);
        assert!(database_version(0).is_err());
        assert!(database_version(i32::MAX as u32 + 1).is_err());

        assert_eq!(stored_version(1).unwrap(), 1);
        assert!(stored_version(0).is_err());
        assert!(stored_version(-1).is_err());
    }
}
