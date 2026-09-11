use sqlx::Postgres;
use sqlx::Transaction;
use uuid::Uuid;

use crate::access::audit_events::record_success;
use crate::access::audit_events::record_success_at;
use crate::crypto::envelope::EncryptedSecret;
use crate::database::error::Error as PersistenceError;
use crate::database::idempotency::Outcome;
use crate::database::idempotency::Replayable;
use crate::database::idempotency::ReplayableIdempotencyClaim;
use crate::database::idempotency::Response;
use crate::database::idempotency::claim_idempotency;
use crate::database::idempotency::claim_replayable_idempotency;
use crate::database::idempotency::complete_idempotency;
use crate::database::idempotency::complete_replayable_idempotency;
use crate::runtime::publication::compiler::compile_and_publish_runtime_in_transaction;
use crate::runtime::publication::compiler::prepare_runtime_mutation;

use crate::protocols::canonical::identity::Surface;
use crate::providers::error::Error;
use crate::providers::record_validation::database_version;
use crate::runtime::publication::PublishedRuntimeRelease;
use chrono::DateTime;
use chrono::Utc;

#[derive(Debug)]
pub struct NewProviderDraft {
    pub provider_id: Uuid,
    pub credential_id: Option<Uuid>,
    pub model_id: Option<Uuid>,
    pub name: String,
    pub configuration: crate::providers::configuration::ProviderConfiguration,

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
    pub release: PublishedRuntimeRelease,
}

pub async fn create_provider_draft<F>(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    provider: NewProviderDraft,
    replay: Replayable<'_>,
    build_response: F,
) -> Result<Outcome<ProviderDraftCreated>, Error>
where
    F: FnOnce(&ProviderDraftCreated) -> Result<Response, PersistenceError>,
{
    let mut transaction = pool.begin().await?;
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
            return Ok(Outcome::Replayed(response));
        }
        ReplayableIdempotencyClaim::Conflict => {
            transaction.rollback().await?;
            return Err(Error::IdempotencyConflict);
        }
        ReplayableIdempotencyClaim::InProgress => {
            transaction.rollback().await?;
            return Err(Error::IdempotencyInProgress);
        }
    }
    validate_initial_provider_records(&provider)?;
    provider
        .configuration
        .options
        .validate(provider.configuration.kind)
        .map_err(Error::Invalid)?;
    let now = chrono::Utc::now();
    let etag = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers \
             (id, name, kind, state, endpoint, cloud_region, cloud_project, deployment, \
              api_version, auth_mode, options, connector_ready, etag, created_by, created_at, \
              updated_at) \
             VALUES ($1, $2, $3, 'draft'::provider_state, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)",
    )
    .bind(provider.provider_id)
    .bind(provider.name.trim())
    .bind(provider.configuration.kind.as_str())
    .bind(provider.configuration.endpoint.as_deref())
    .bind(provider.configuration.cloud_region.as_deref())
    .bind(provider.configuration.cloud_project.as_deref())
    .bind(provider.configuration.deployment.as_deref())
    .bind(provider.configuration.api_version.as_deref())
    .bind(provider.configuration.auth_mode.as_str())
    .bind(sqlx::types::Json(&provider.configuration.options))
    .bind(provider.connector_ready)
    .bind(etag)
    .bind(provider.actor)
    .bind(now)
    .execute(&mut *transaction)
    .await?;
    sqlx::query("INSERT INTO provider_credential_slots(id, provider_id, name, is_default) VALUES ($1, $1, 'Default', true)")
        .bind(provider.provider_id).execute(&mut *transaction).await?;
    insert_initial_provider_credential(&mut transaction, &provider, now).await?;
    sqlx::query("UPDATE provider_credential_slots SET selected_version_id = $1 WHERE provider_id = $2 AND is_default")
        .bind(provider.credential_id).bind(provider.provider_id).execute(&mut *transaction).await?;
    sqlx::query("UPDATE provider_credential_versions SET slot_id = $1 WHERE provider_id = $1 AND slot_id IS NULL")
        .bind(provider.provider_id).execute(&mut *transaction).await?;
    insert_initial_provider_model(&mut transaction, &provider, now).await?;
    record_success_at(
        &mut *transaction,
        provenance,
        Some(provider.actor),
        "provider.create_draft",
        "provider",
        provider.provider_id,
        now,
    )
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
    Ok(Outcome::Executed {
        value: created,
        response,
    })
}

fn validate_initial_provider_records(provider: &NewProviderDraft) -> Result<(), Error> {
    if provider.credential.is_some() != provider.credential_id.is_some() {
        return Err(Error::InvalidCredential);
    }
    if provider.model.is_some() != provider.model_id.is_some()
        || provider.model.is_some() != provider.display_name.is_some()
        || (provider.model.is_none() && (provider.model_enabled || provider.surface.is_some()))
    {
        return Err(Error::ProviderIncomplete);
    }
    Ok(())
}

pub async fn activate_provider(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    provider_id: Uuid,
    expected_etag: Uuid,
    actor: Uuid,
    idempotency_key: &str,
) -> Result<ProviderActivated, Error> {
    let new_etag = Uuid::now_v7();
    let mut transaction = pool
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
        return Err(Error::IdempotencyConflict);
    }
    let provider = lock_activatable_provider(&mut transaction, provider_id, expected_etag).await?;
    let revision_id = snapshot_provider_revision(
        &mut transaction,
        provider_id,
        expected_etag,
        actor,
        &provider,
    )
    .await?;
    crate::providers::pool_store::snapshot(&mut transaction, provider_id, revision_id).await?;
    reject_incompatible_media_jobs(&mut transaction, provider_id, revision_id).await?;
    reject_unroutable_activation(&mut transaction, provider_id, revision_id).await?;
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

    let previous_credential: Option<Uuid> = provider.previously_activated_credential_id;
    let activated_credential: Option<Uuid> = provider.active_credential_version_id;
    if previous_credential.is_some() && previous_credential != activated_credential {
        sqlx::query(
            "UPDATE provider_credential_versions SET revoked_at = COALESCE(revoked_at, now()) \
                 WHERE id = $1 AND provider_id = $2 AND NOT EXISTS (
                   SELECT 1 FROM async_media_jobs j JOIN provider_revisions pr ON pr.id=j.provider_revision_id
                   WHERE j.provider_id=$2 AND j.lifecycle_state <> 'deleted'
                     AND COALESCE(j.credential_version_id,pr.credential_version_id)=$1
                 )",
        )
        .bind(previous_credential)
        .bind(provider_id)
        .execute(&mut *transaction)
        .await?;
    }
    record_success(
        &mut *transaction,
        provenance,
        actor,
        "provider.activate",
        "provider",
        provider_id,
    )
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

/// The activated provider revision's connector snapshot, taken under the
/// provider row lock that gates activation.
struct ActivationSource {
    name: String,
    kind: String,
    endpoint: Option<String>,
    cloud_region: Option<String>,
    cloud_project: Option<String>,
    deployment: Option<String>,
    api_version: Option<String>,
    auth_mode: String,
    options: sqlx::types::Json<crate::providers::options::ConnectionOptions>,
    connector_ready: bool,
    active_credential_version_id: Option<Uuid>,
    previously_activated_credential_id: Option<Uuid>,
}

async fn lock_activatable_provider(
    transaction: &mut Transaction<'_, Postgres>,
    provider_id: Uuid,
    expected_etag: Uuid,
) -> Result<ActivationSource, Error> {
    let provider = sqlx::query_as::<_, LockActivatableProviderRow>(
        "SELECT p.name, p.kind, p.state::text AS \"state\", p.endpoint, p.cloud_region, \
                p.cloud_project, p.deployment, p.api_version, p.auth_mode, p.options, \
                p.connector_ready, p.etag, p.active_credential_version_id, \
                ar.credential_version_id AS \"previously_activated_credential_id\", \
                (p.last_probe_status = 'succeeded' AND p.last_probe_at IS NOT NULL \
                 AND p.last_probe_at >= p.updated_at) AS \"probe_ready\", \
                (EXISTS (SELECT 1 FROM provider_credential_slots s \
                         WHERE s.provider_id = p.id AND s.is_default AND NOT s.enabled) OR \
                 (p.auth_mode IN ('adc', 'default_chain', 'none') \
                  AND p.active_credential_version_id IS NULL) OR EXISTS ( \
                     SELECT 1 FROM provider_credential_versions cv \
                     WHERE cv.id = p.active_credential_version_id \
                       AND cv.provider_id = p.id AND cv.revoked_at IS NULL)) AS \"credential_ready\", \
                EXISTS (SELECT 1 FROM provider_models pm \
                        WHERE pm.provider_id = p.id AND pm.enabled) AS \"has_model\", \
                NOT EXISTS ( \
                  SELECT 1 FROM provider_models pm \
                  WHERE pm.provider_id = p.id AND pm.enabled AND ( \
                    NOT EXISTS (SELECT 1 FROM model_capabilities mc \
                                WHERE mc.provider_model_id = pm.id) OR \
                    EXISTS (SELECT 1 FROM model_capabilities mc \
                            WHERE mc.provider_model_id = pm.id \
                              AND mc.source <> 'certified'))) AS \"capabilities_ready\" \
         FROM providers p \
         LEFT JOIN provider_revisions ar ON ar.id = p.active_revision_id \
         WHERE p.id = $1 FOR UPDATE OF p",
    )
    .bind(provider_id)
    .fetch_optional(&mut **transaction)
    .await?
    .ok_or(Error::ProviderNotFound)?;
    if provider.etag != expected_etag {
        return Err(Error::PreconditionFailed);
    }
    if provider.state != "draft"
        || !provider.connector_ready
        || !provider.probe_ready
        || !provider.credential_ready
        || !provider.has_model
        || !provider.capabilities_ready
    {
        return Err(Error::ProviderIncomplete);
    }
    Ok(ActivationSource {
        name: provider.name,
        kind: provider.kind,
        endpoint: provider.endpoint,
        cloud_region: provider.cloud_region,
        cloud_project: provider.cloud_project,
        deployment: provider.deployment,
        api_version: provider.api_version,
        auth_mode: provider.auth_mode,
        options: provider.options,
        connector_ready: provider.connector_ready,
        active_credential_version_id: provider.active_credential_version_id,
        previously_activated_credential_id: provider.previously_activated_credential_id,
    })
}

async fn snapshot_provider_revision(
    transaction: &mut Transaction<'_, Postgres>,
    provider_id: Uuid,
    expected_etag: Uuid,
    actor: Uuid,
    provider: &ActivationSource,
) -> Result<Uuid, Error> {
    let revision: i32 = sqlx::query_scalar::<_, i32>("SELECT COALESCE(max(revision), 0) + 1 AS \"value\" FROM provider_revisions WHERE provider_id = $1")
    .bind(provider_id)
    .fetch_one(&mut **transaction)
    .await?;
    let revision_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO provider_revisions \
         (id, provider_id, revision, name, kind, endpoint, cloud_region, cloud_project, \
          deployment, api_version, auth_mode, options, connector_ready, \
          credential_version_id, source_etag, activated_by) \
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)",
    )
    .bind(revision_id)
    .bind(provider_id)
    .bind(revision)
    .bind(&provider.name)
    .bind(&provider.kind)
    .bind(&provider.endpoint)
    .bind(&provider.cloud_region)
    .bind(&provider.cloud_project)
    .bind(&provider.deployment)
    .bind(&provider.api_version)
    .bind(&provider.auth_mode)
    .bind(&provider.options)
    .bind(provider.connector_ready)
    .bind(provider.active_credential_version_id)
    .bind(expected_etag)
    .bind(actor)
    .execute(&mut **transaction)
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
    .execute(&mut **transaction)
    .await?;
    sqlx::query(
        "INSERT INTO provider_revision_capabilities \
         (provider_revision_model_id, operation, surface, mode, source, certified_at) \
         SELECT prm.id, mc.operation, mc.surface, mc.mode, mc.source, mc.certified_at \
         FROM provider_revision_models prm \
         JOIN model_capabilities mc ON mc.provider_model_id = prm.source_provider_model_id \
         WHERE prm.provider_revision_id = $1",
    )
    .bind(revision_id)
    .execute(&mut **transaction)
    .await?;
    Ok(revision_id)
}

async fn reject_incompatible_media_jobs(
    transaction: &mut Transaction<'_, Postgres>,
    provider_id: Uuid,
    revision_id: Uuid,
) -> Result<(), Error> {
    let incompatible_media_job: Option<Uuid> = sqlx::query_scalar::<_, uuid::Uuid>(
        "SELECT j.id
         FROM async_media_jobs j
         JOIN providers p ON p.id = j.provider_id
         LEFT JOIN provider_revisions authority
           ON authority.id = j.provider_revision_id
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
             OR ($3::jsonb || authority.options) - 'limits'
                  IS DISTINCT FROM ($3::jsonb || candidate.options) - 'limits'
             OR authority.auth_mode IS DISTINCT FROM candidate.auth_mode
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
    )
    .bind(provider_id)
    .bind(revision_id)
    .bind(sqlx::types::Json(
        crate::providers::options::ConnectionOptions::default(),
    ))
    .fetch_optional(&mut **transaction)
    .await?;
    if let Some(job_id) = incompatible_media_job {
        return Err(Error::ProviderMediaJobIncompatible { job_id });
    }
    Ok(())
}

async fn reject_unroutable_activation(
    transaction: &mut Transaction<'_, Postgres>,
    provider_id: Uuid,
    revision_id: Uuid,
) -> Result<(), Error> {
    let uncovered_route_operation: Option<String> = sqlx::query_scalar::<_, String>(
        "SELECT concat(r.slug, '/', rro.operation) AS \"value\" \
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
    .fetch_optional(&mut **transaction)
    .await?;
    if uncovered_route_operation.is_some() {
        return Err(Error::ProviderIncomplete);
    }
    // Preserve explicitly published targets even when another target covers
    // the operation. Require a reviewed route edit before a model disappears
    // from its provider's active revision.
    let orphaned_route_target = sqlx::query_as::<_, RejectUnroutableActivationRow>(
        "SELECT r.slug AS \"route_slug\", \
                concat(p.name, '/', pm.upstream_model) AS \"target\" \
         FROM routes r \
         JOIN LATERAL (SELECT id FROM route_revisions \
                       WHERE route_id = r.id ORDER BY revision DESC LIMIT 1) rr ON true \
         JOIN route_revision_targets rt ON rt.route_revision_id = rr.id \
         JOIN provider_models pm ON pm.id = rt.provider_model_id \
         JOIN providers p ON p.id = pm.provider_id \
         LEFT JOIN provider_revision_models prm \
           ON prm.source_provider_model_id = pm.id AND prm.provider_revision_id = $2 \
         WHERE p.id = $1 AND (prm.id IS NULL OR NOT prm.enabled) \
         ORDER BY r.slug, rt.position LIMIT 1",
    )
    .bind(provider_id)
    .bind(revision_id)
    .fetch_optional(&mut **transaction)
    .await?;
    if let Some(orphaned) = orphaned_route_target {
        return Err(Error::Invalid(format!(
            "route {} targets {}, which this provider revision does not enable",
            orphaned.route_slug, orphaned.target
        )));
    }
    Ok(())
}

async fn insert_initial_provider_credential(
    transaction: &mut Transaction<'_, Postgres>,
    provider: &NewProviderDraft,
    now: DateTime<Utc>,
) -> Result<(), Error> {
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
        .execute(&mut **transaction)
        .await?;
        sqlx::query("UPDATE providers SET active_credential_version_id = $1 WHERE id = $2")
            .bind(credential_id)
            .bind(provider.provider_id)
            .execute(&mut **transaction)
            .await?;
    }
    Ok(())
}

async fn insert_initial_provider_model(
    transaction: &mut Transaction<'_, Postgres>,
    provider: &NewProviderDraft,
    now: DateTime<Utc>,
) -> Result<(), Error> {
    if let (Some(model_id), Some(model), Some(display_name)) =
        (provider.model_id, &provider.model, &provider.display_name)
    {
        sqlx::query(
            "INSERT INTO provider_models \
             (id, provider_id, upstream_model, display_name, enabled, discovered_at, created_at) \
             VALUES ($1, $2, $3, $4, $5, $6, $6)",
        )
        .bind(model_id)
        .bind(provider.provider_id)
        .bind(model.trim())
        .bind(display_name.trim())
        .bind(provider.model_enabled)
        .bind(now)
        .execute(&mut **transaction)
        .await?;
    }
    if let (Some(surface), Some(model_id)) = (&provider.surface, provider.model_id) {
        let embeddings = provider.configuration.options.vendor_id.as_deref() == Some("voyage");
        for mode in if embeddings {
            &["unary"][..]
        } else {
            &["unary", "streaming"][..]
        } {
            sqlx::query(
                "INSERT INTO model_capabilities \
                 (provider_model_id, operation, surface, mode, source, certified_at) \
                 VALUES ($1, $4, $2, $3, 'declared', NULL)",
            )
            .bind(model_id)
            .bind(surface.as_str())
            .bind(*mode)
            .bind(if embeddings {
                "embeddings"
            } else {
                "generation"
            })
            .execute(&mut **transaction)
            .await?;
        }
    }
    Ok(())
}

#[derive(sqlx::FromRow)]
struct LockActivatableProviderRow {
    name: String,
    kind: String,
    state: String,
    endpoint: Option<String>,
    cloud_region: Option<String>,
    cloud_project: Option<String>,
    deployment: Option<String>,
    api_version: Option<String>,
    auth_mode: String,
    options: sqlx::types::Json<crate::providers::options::ConnectionOptions>,
    connector_ready: bool,
    etag: uuid::Uuid,
    active_credential_version_id: Option<uuid::Uuid>,
    previously_activated_credential_id: Option<uuid::Uuid>,
    probe_ready: bool,
    credential_ready: bool,
    has_model: bool,
    capabilities_ready: bool,
}

#[derive(sqlx::FromRow)]
struct RejectUnroutableActivationRow {
    route_slug: String,
    target: String,
}
