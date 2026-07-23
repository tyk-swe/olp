use olp_domain::{ProviderId, ProviderKind, RuntimeSnapshot};
use sqlx::FromRow;
use uuid::Uuid;

use crate::{
    EncryptedSecret, IdempotencyOutcome, IdempotencyResponse, PersistenceError, PgStore,
    ReplayableIdempotency,
    runtime_compiler::{compile_and_publish_runtime_in_transaction, prepare_runtime_mutation},
    store::{
        ReplayableIdempotencyClaim, claim_idempotency, claim_replayable_idempotency,
        complete_idempotency, complete_replayable_idempotency,
    },
};

use super::{
    ConfigurationError, NewProviderDraft, ProviderActivated, ProviderDraftCreated,
    RuntimeProviderConfiguration,
};

impl PgStore {
    /// Loads the release-exact connector configuration and credential named by
    /// a verified runtime sidecar. Mutable configuration drafts are deliberately not
    /// consulted, so testing a replacement endpoint or credential cannot alter
    /// the transport used by the last activated provider revision.
    pub async fn runtime_provider_configurations(
        &self,
        snapshot: &RuntimeSnapshot,
    ) -> Result<Vec<RuntimeProviderConfiguration>, ConfigurationError> {
        let mut records = Vec::with_capacity(snapshot.providers.len());
        for runtime_provider in snapshot.providers.values() {
            let expected_credential = runtime_provider
                .active_credential
                .map(|credential| credential.as_uuid());
            let row = sqlx::query_as!(
                RuntimeProviderRow,
                "SELECT rpc.provider_id AS id, rpc.kind, rpc.endpoint, rpc.cloud_region, \
                        rpc.cloud_project, rpc.deployment, rpc.api_version, rpc.auth_mode, \
                        cv.id AS \"credential_id?\", cv.version AS \"credential_version?\", \
                        cv.ciphertext AS \"ciphertext?\", cv.nonce AS \"nonce?\", \
                        cv.master_key_version AS \"master_key_version?\" \
                 FROM runtime_generation_provider_configs rpc \
                 JOIN providers p ON p.id = rpc.provider_id \
                 LEFT JOIN provider_credential_versions cv \
                   ON cv.id = rpc.active_credential_version_id AND cv.revoked_at IS NULL \
                 WHERE rpc.provider_id = $1 AND rpc.runtime_generation_id = $3 \
                   AND rpc.active_credential_version_id IS NOT DISTINCT FROM $2 \
                   AND p.active_revision_id IS NOT NULL \
                   AND p.state <> 'disabled'::provider_state \
                   AND (rpc.active_credential_version_id IS NULL OR cv.id IS NOT NULL)",
                runtime_provider.id.as_uuid(),
                expected_credential,
                snapshot.generation.id.as_uuid()
            )
            .fetch_optional(self.pool())
            .await?
            .ok_or(ConfigurationError::InvalidCredential)?;
            let stored_kind = parse_provider_kind(row.kind.as_str())?;
            if stored_kind != runtime_provider.kind {
                return Err(ConfigurationError::InvalidCredential);
            }
            records.push(runtime_provider_configuration_from_row(row)?);
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
        let now = chrono::Utc::now();
        let etag = Uuid::now_v7();
        sqlx::query!(
            "INSERT INTO providers \
             (id, name, kind, state, endpoint, cloud_region, cloud_project, deployment, \
              api_version, auth_mode, connector_ready, etag, created_by, created_at, updated_at) \
             VALUES ($1, $2, $3, 'draft'::provider_state, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13)",
        provider.provider_id, provider.name.trim(), provider.kind.as_str(), provider.endpoint.as_deref(), provider.cloud_region.as_deref(), provider.cloud_project.as_deref(), provider.deployment.as_deref(), provider.api_version.as_deref(), provider.auth_mode.as_str(), provider.connector_ready, etag, provider.actor, now)
        .execute(&mut *transaction)
        .await?;
        if let (Some(credential_id), Some(credential)) =
            (provider.credential_id, provider.credential.as_ref())
        {
            let master_key_version = database_version(credential.key_version)?;
            sqlx::query!(
                "INSERT INTO provider_credential_versions \
                 (id, provider_id, version, ciphertext, nonce, master_key_version, created_by, created_at) \
                 VALUES ($1, $2, 1, $3, $4, $5, $6, $7)",
            credential_id, provider.provider_id, &credential.ciphertext, credential.nonce.to_vec(), master_key_version, provider.actor, now)
            .execute(&mut *transaction)
            .await?;
            sqlx::query!(
                "UPDATE providers SET active_credential_version_id = $1 WHERE id = $2",
                credential_id,
                provider.provider_id
            )
            .execute(&mut *transaction)
            .await?;
        }
        if let (Some(model_id), Some(model), Some(display_name)) =
            (provider.model_id, &provider.model, &provider.display_name)
        {
            sqlx::query!(
                "INSERT INTO provider_models \
                 (id, provider_id, upstream_model, display_name, enabled, discovered_at, created_at) \
                 VALUES ($1, $2, $3, $4, $5, $6, $6)",
            model_id, provider.provider_id, model.trim(), display_name.trim(), provider.model_enabled, now)
            .execute(&mut *transaction)
            .await?;
        }
        if let (Some(surface), Some(model_id)) = (&provider.surface, provider.model_id) {
            for mode in ["unary", "streaming"] {
                sqlx::query!(
                    "INSERT INTO model_capabilities \
                     (provider_model_id, operation, surface, mode, source, certified_at) \
                     VALUES ($1, 'generation', $2, $3, 'declared', NULL)",
                    model_id,
                    surface.as_str(),
                    mode
                )
                .execute(&mut *transaction)
                .await?;
            }
        }
        sqlx::query!(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'provider.create_draft', 'provider', $3, 'success', $4)",
            Uuid::now_v7(),
            provider.actor,
            provider.provider_id.to_string(),
            now
        )
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
        let provider = sqlx::query!(
            "SELECT p.name, p.kind, p.state::text AS \"state!\", p.endpoint, p.cloud_region, \
                    p.cloud_project, p.deployment, p.api_version, p.auth_mode, \
                    p.connector_ready, p.etag, p.active_credential_version_id, \
                    ar.credential_version_id AS \"previously_activated_credential_id?\", \
                    (p.last_probe_status = 'succeeded' AND p.last_probe_at IS NOT NULL \
                     AND p.last_probe_at >= p.updated_at) AS \"probe_ready!\", \
                    ((p.auth_mode IN ('adc', 'default_chain') \
                      AND p.active_credential_version_id IS NULL) OR EXISTS ( \
                         SELECT 1 FROM provider_credential_versions cv \
                         WHERE cv.id = p.active_credential_version_id \
                           AND cv.provider_id = p.id AND cv.revoked_at IS NULL)) AS \"credential_ready!\", \
                    EXISTS (SELECT 1 FROM provider_models pm \
                            WHERE pm.provider_id = p.id AND pm.enabled) AS \"has_model!\", \
                    NOT EXISTS ( \
                      SELECT 1 FROM provider_models pm \
                      WHERE pm.provider_id = p.id AND pm.enabled AND ( \
                        NOT EXISTS (SELECT 1 FROM model_capabilities mc \
                                    WHERE mc.provider_model_id = pm.id) OR \
                        EXISTS (SELECT 1 FROM model_capabilities mc \
                                WHERE mc.provider_model_id = pm.id \
                                  AND mc.source <> 'certified'))) AS \"capabilities_ready!\" \
             FROM providers p \
             LEFT JOIN provider_revisions ar ON ar.id = p.active_revision_id \
             WHERE p.id = $1 FOR UPDATE OF p",
        provider_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(ConfigurationError::ProviderNotFound)?;
        if provider.etag != expected_etag {
            return Err(ConfigurationError::PreconditionFailed);
        }
        if provider.state != "draft"
            || !provider.connector_ready
            || !provider.probe_ready
            || !provider.credential_ready
            || !provider.has_model
            || !provider.capabilities_ready
        {
            return Err(ConfigurationError::ProviderIncomplete);
        }

        // Media reservations are short RowExclusive transactions. Holding a
        // table SHARE lock makes this activation decision atomic with respect
        // to new upstream jobs on every gateway replica.
        sqlx::query!("LOCK TABLE async_media_jobs IN SHARE MODE")
            .execute(&mut *transaction)
            .await?;

        let revision: i32 = sqlx::query_scalar!(
            "SELECT COALESCE(max(revision), 0) + 1 AS \"value!\" FROM provider_revisions WHERE provider_id = $1",
            provider_id
        )
        .fetch_one(&mut *transaction)
        .await?;
        let revision_id = Uuid::now_v7();
        sqlx::query!(
            "INSERT INTO provider_revisions \
             (id, provider_id, revision, name, kind, endpoint, cloud_region, cloud_project, \
              deployment, api_version, auth_mode, connector_ready, credential_version_id, \
              source_etag, activated_by) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)",
            revision_id,
            provider_id,
            revision,
            provider.name,
            provider.kind,
            provider.endpoint,
            provider.cloud_region,
            provider.cloud_project,
            provider.deployment,
            provider.api_version,
            provider.auth_mode,
            provider.connector_ready,
            provider.active_credential_version_id,
            expected_etag,
            actor
        )
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "INSERT INTO provider_revision_models \
             (id, provider_revision_id, source_provider_model_id, upstream_model, \
              display_name, enabled, discovered_at) \
             SELECT uuidv7(), $1, pm.id, pm.upstream_model, pm.display_name, pm.enabled, \
                    pm.discovered_at FROM provider_models pm WHERE pm.provider_id = $2",
            revision_id,
            provider_id
        )
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "INSERT INTO provider_revision_capabilities \
             (provider_revision_model_id, operation, surface, mode, source, certified_at) \
             SELECT prm.id, mc.operation, mc.surface, mc.mode, mc.source, mc.certified_at \
             FROM provider_revision_models prm \
             JOIN model_capabilities mc ON mc.provider_model_id = prm.source_provider_model_id \
             WHERE prm.provider_revision_id = $1",
            revision_id
        )
        .execute(&mut *transaction)
        .await?;
        let incompatible_media_job: Option<Uuid> = sqlx::query_scalar!(
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
                           AND prc.surface = j.surface
                           AND prc.mode = 'unary' AND prc.source = 'certified'
                       )
                     )
                 )
               )
             ORDER BY j.created_at, j.id LIMIT 1",
            provider_id,
            revision_id
        )
        .fetch_optional(&mut *transaction)
        .await?;
        if incompatible_media_job.is_some() {
            return Err(ConfigurationError::ProviderIncomplete);
        }
        let uncovered_route_operation: Option<String> = sqlx::query_scalar!(
            "SELECT concat(r.slug, '/', rro.operation) AS \"value!\" \
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
            provider_id,
            revision_id
        )
        .fetch_optional(&mut *transaction)
        .await?;
        if uncovered_route_operation.is_some() {
            return Err(ConfigurationError::ProviderIncomplete);
        }
        sqlx::query!(
            "UPDATE providers SET state = 'active'::provider_state, active_revision_id = $1, \
                    etag = $2, updated_at = now() WHERE id = $3 AND etag = $4",
            revision_id,
            new_etag,
            provider_id,
            expected_etag
        )
        .execute(&mut *transaction)
        .await?;

        let previous_credential: Option<Uuid> = provider.previously_activated_credential_id;
        let activated_credential: Option<Uuid> = provider.active_credential_version_id;
        if previous_credential.is_some() && previous_credential != activated_credential {
            sqlx::query!(
                "UPDATE provider_credential_versions SET revoked_at = COALESCE(revoked_at, now()) \
                 WHERE id = $1 AND provider_id = $2",
                previous_credential,
                provider_id
            )
            .execute(&mut *transaction)
            .await?;
        }
        sqlx::query!(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome) \
             VALUES ($1, $2, 'provider.activate', 'provider', $3, 'success')",
            Uuid::now_v7(),
            actor,
            provider_id.to_string()
        )
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
}

#[derive(Debug, FromRow)]
struct RuntimeProviderRow {
    id: Uuid,
    kind: String,
    endpoint: Option<String>,
    cloud_region: Option<String>,
    cloud_project: Option<String>,
    deployment: Option<String>,
    api_version: Option<String>,
    auth_mode: String,
    credential_id: Option<Uuid>,
    credential_version: Option<i32>,
    ciphertext: Option<Vec<u8>>,
    nonce: Option<Vec<u8>>,
    master_key_version: Option<i32>,
}

fn runtime_provider_configuration_from_row(
    row: RuntimeProviderRow,
) -> Result<RuntimeProviderConfiguration, ConfigurationError> {
    let credential_id: Option<Uuid> = row.credential_id;
    let credential_version = row.credential_version.map(stored_version).transpose()?;
    let nonce = row.nonce;
    let ciphertext = row.ciphertext;
    let key_version = row.master_key_version.map(stored_version).transpose()?;
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
    Ok(RuntimeProviderConfiguration {
        provider_id: ProviderId::from_uuid(row.id),
        kind: parse_provider_kind(row.kind.as_str())?,
        endpoint: row.endpoint,
        cloud_region: row.cloud_region,
        cloud_project: row.cloud_project,
        deployment: row.deployment,
        api_version: row.api_version,
        auth_mode: row.auth_mode.parse().map_err(|_| {
            PersistenceError::InvalidStoredValue("runtime provider authentication mode")
        })?,
        credential_id,
        credential_version,
        encrypted_credential: encrypted,
    })
}

fn parse_provider_kind(value: &str) -> Result<ProviderKind, ConfigurationError> {
    value
        .parse()
        .map_err(|_| ConfigurationError::InvalidCredential)
}

pub(super) fn database_version(version: u32) -> Result<i32, ConfigurationError> {
    i32::try_from(version)
        .ok()
        .filter(|version| *version > 0)
        .ok_or(ConfigurationError::InvalidCredential)
}

pub(super) fn stored_version(version: i32) -> Result<u32, ConfigurationError> {
    u32::try_from(version)
        .ok()
        .filter(|version| *version > 0)
        .ok_or(ConfigurationError::InvalidCredential)
}
