use crate::access::audit_events::record_success;
use crate::database::idempotency::claim_idempotency;
use crate::database::idempotency::complete_idempotency;
use crate::database::page::ConfigurationPage;
use crate::database::query::split_page;
use crate::providers::error::Error;
use crate::providers::queries::CapabilityRow;
use crate::providers::queries::capability_from_row;
use crate::providers::queries::checked_configuration_count;
use crate::providers::queries::lock_provider;
use crate::providers::record_validation::checked_limit;
use crate::providers::record_validation::enforce_provider_revision_diff_limit;
use crate::providers::records::CapabilityRecord;
use crate::providers::records::PROVIDER_REVISION_DIFF_CAPABILITY_LIMIT;
use crate::providers::records::PROVIDER_REVISION_DIFF_MODEL_LIMIT;
use crate::providers::records::ProviderModelRecord;
use crate::providers::records::ProviderRecord;
use crate::providers::records::ProviderRevisionDiff;
use crate::providers::records::ProviderRevisionRecord;
use chrono::DateTime;
use chrono::Utc;
use sqlx::PgPool;
use std::collections::BTreeMap;
use std::collections::BTreeSet;
use uuid::Uuid;

pub async fn list_provider_revisions(
    pool: &sqlx::PgPool,
    provider_id: Uuid,
    cursor: Option<Uuid>,
    limit: i64,
) -> Result<ConfigurationPage<ProviderRevisionRecord>, Error> {
    let limit = checked_limit(limit)?;
    let exists: bool = sqlx::query_scalar::<_, bool>(
        "SELECT EXISTS (SELECT 1 FROM providers WHERE id = $1) AS \"value\"",
    )
    .bind(provider_id)
    .fetch_one(pool)
    .await?;
    if !exists {
        return Err(Error::NotFound);
    }
    let before_revision: Option<i32> = match cursor {
        Some(cursor) => Some(
            sqlx::query_scalar::<_, i32>(
                "SELECT revision FROM provider_revisions WHERE provider_id = $1 AND id = $2",
            )
            .bind(provider_id)
            .bind(cursor)
            .fetch_optional(pool)
            .await?
            .ok_or_else(|| {
                Error::Invalid("provider-revision pagination cursor is invalid".to_owned())
            })?,
        ),
        None => None,
    };
    let rows = sqlx::query_as::<_, ProviderRevisionRow>(
        "SELECT pr.id, pr.provider_id, pr.revision, pr.name, pr.kind, pr.endpoint, \
                    pr.cloud_region, pr.cloud_project, pr.deployment, pr.api_version, \
                    pr.auth_mode, pr.connector_ready, pr.credential_version_id, \
                    cv.version AS \"credential_version\", pr.source_etag, pr.activated_by, \
                    pr.activated_at, stats.model_count AS \"model_count\", \
                    stats.enabled_model_count AS \"enabled_model_count\", \
                    stats.capability_count AS \"capability_count\", \
                    stats.certified_capability_count AS \"certified_capability_count\" \
             FROM provider_revisions pr \
             LEFT JOIN provider_credential_versions cv ON cv.id = pr.credential_version_id \
             LEFT JOIN LATERAL ( \
                 SELECT COUNT(DISTINCT prm.id)::bigint AS model_count, \
                        COUNT(DISTINCT prm.id) FILTER (WHERE prm.enabled)::bigint \
                          AS enabled_model_count, \
                        COUNT(prc.provider_revision_model_id)::bigint AS capability_count, \
                        COUNT(prc.provider_revision_model_id) \
                          FILTER (WHERE prc.source = 'certified')::bigint \
                          AS certified_capability_count \
                 FROM provider_revision_models prm \
                 LEFT JOIN provider_revision_capabilities prc \
                   ON prc.provider_revision_model_id = prm.id \
                 WHERE prm.provider_revision_id = pr.id \
             ) stats ON true \
             WHERE pr.provider_id = $1 \
             AND ($2::int IS NULL OR pr.revision < $2) \
             ORDER BY pr.revision DESC LIMIT $3",
    )
    .bind(provider_id)
    .bind(before_revision)
    .bind(limit + 1)
    .fetch_all(pool)
    .await?;
    let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.id);
    let revisions = rows
        .into_iter()
        .map(provider_revision_from_row)
        .collect::<Result<Vec<_>, _>>()?;
    Ok(ConfigurationPage {
        items: revisions,
        next_cursor,
    })
}

pub async fn get_provider_revision(
    pool: &sqlx::PgPool,
    provider_id: Uuid,
    revision_id: Uuid,
) -> Result<ProviderRevisionRecord, Error> {
    let row = sqlx::query_as::<_, ProviderRevisionRow>(
        "SELECT pr.id, pr.provider_id, pr.revision, pr.name, pr.kind, pr.endpoint, \
                    pr.cloud_region, pr.cloud_project, pr.deployment, pr.api_version, \
                    pr.auth_mode, pr.connector_ready, pr.credential_version_id, \
                    cv.version AS \"credential_version\", pr.source_etag, pr.activated_by, \
                    pr.activated_at, stats.model_count AS \"model_count\", \
                    stats.enabled_model_count AS \"enabled_model_count\", \
                    stats.capability_count AS \"capability_count\", \
                    stats.certified_capability_count AS \"certified_capability_count\" \
             FROM provider_revisions pr \
             LEFT JOIN provider_credential_versions cv ON cv.id = pr.credential_version_id \
             LEFT JOIN LATERAL ( \
                 SELECT COUNT(DISTINCT prm.id)::bigint AS model_count, \
                        COUNT(DISTINCT prm.id) FILTER (WHERE prm.enabled)::bigint \
                          AS enabled_model_count, \
                        COUNT(prc.provider_revision_model_id)::bigint AS capability_count, \
                        COUNT(prc.provider_revision_model_id) \
                          FILTER (WHERE prc.source = 'certified')::bigint \
                          AS certified_capability_count \
                 FROM provider_revision_models prm \
                 LEFT JOIN provider_revision_capabilities prc \
                   ON prc.provider_revision_model_id = prm.id \
                 WHERE prm.provider_revision_id = pr.id \
             ) stats ON true \
             WHERE pr.provider_id = $1 AND pr.id = $2",
    )
    .bind(provider_id)
    .bind(revision_id)
    .fetch_optional(pool)
    .await?
    .ok_or(Error::NotFound)?;
    provider_revision_from_row(row)
}

pub async fn list_provider_revision_models(
    pool: &sqlx::PgPool,
    provider_id: Uuid,
    revision_id: Uuid,
    cursor: Option<Uuid>,
    limit: i64,
) -> Result<ConfigurationPage<ProviderModelRecord>, Error> {
    let limit = checked_limit(limit)?;
    ensure_provider_revision_exists(pool, provider_id, revision_id).await?;
    let rows = sqlx::query_as::<_, ProviderRevisionModelRow>(
        "SELECT id AS revision_model_id, source_provider_model_id, upstream_model, \
                    display_name, enabled, discovered_at \
             FROM provider_revision_models WHERE provider_revision_id = $1 \
               AND ($2::uuid IS NULL OR id > $2) ORDER BY id LIMIT $3",
    )
    .bind(revision_id)
    .bind(cursor)
    .bind(limit + 1)
    .fetch_all(pool)
    .await?;
    let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.revision_model_id);
    let items =
        crate::providers::revisions::provider_revision_models_from_rows(pool, rows, None).await?;
    Ok(ConfigurationPage { items, next_cursor })
}

async fn provider_revision_models_from_rows(
    pool: &sqlx::PgPool,
    rows: Vec<ProviderRevisionModelRow>,
    capability_limit: Option<usize>,
) -> Result<Vec<ProviderModelRecord>, Error> {
    let revision_model_ids = rows
        .iter()
        .map(|row| row.revision_model_id)
        .collect::<Vec<_>>();
    let capability_rows = if revision_model_ids.is_empty() {
        Vec::new()
    } else if let Some(limit) = capability_limit {
        sqlx::query_as::<_, RevisionCapabilityRow>(
            "SELECT provider_revision_model_id, operation, surface, mode, source, certified_at \
                 FROM provider_revision_capabilities \
                 WHERE provider_revision_model_id = ANY($1::uuid[]) \
                 ORDER BY provider_revision_model_id, operation, surface, mode LIMIT $2",
        )
        .bind(&revision_model_ids)
        .bind(limit as i64 + 1)
        .fetch_all(pool)
        .await?
    } else {
        sqlx::query_as::<_, RevisionCapabilityRow>(
            "SELECT provider_revision_model_id, operation, surface, mode, source, certified_at \
                 FROM provider_revision_capabilities \
                 WHERE provider_revision_model_id = ANY($1::uuid[]) \
                 ORDER BY provider_revision_model_id, operation, surface, mode",
        )
        .bind(&revision_model_ids)
        .fetch_all(pool)
        .await?
    };
    if let Some(limit) = capability_limit {
        enforce_provider_revision_diff_limit(capability_rows.len(), "capability tuples", limit)?;
    }
    let mut capabilities = BTreeMap::<Uuid, Vec<CapabilityRecord>>::new();
    for row in capability_rows {
        let (provider_revision_model_id, capability) = row.split();
        capabilities
            .entry(provider_revision_model_id)
            .or_default()
            .push(capability_from_row(capability)?);
    }
    Ok(rows
        .into_iter()
        .map(|row| {
            let revision_model_id = row.revision_model_id;
            ProviderModelRecord {
                id: row.source_provider_model_id,
                upstream_model: row.upstream_model,
                display_name: row.display_name,
                enabled: row.enabled,
                discovered_at: row.discovered_at,
                capabilities: capabilities.remove(&revision_model_id).unwrap_or_default(),
            }
        })
        .collect())
}

pub(crate) async fn all_provider_revision_models(
    pool: &sqlx::PgPool,
    revision_id: Uuid,
) -> Result<Vec<ProviderModelRecord>, Error> {
    let rows = sqlx::query_as::<_, ProviderRevisionModelRow>(
        "SELECT id AS revision_model_id, source_provider_model_id, upstream_model, \
                    display_name, enabled, discovered_at \
             FROM provider_revision_models WHERE provider_revision_id = $1 ORDER BY id LIMIT $2",
    )
    .bind(revision_id)
    .bind(PROVIDER_REVISION_DIFF_MODEL_LIMIT as i64 + 1)
    .fetch_all(pool)
    .await?;
    enforce_provider_revision_diff_limit(rows.len(), "models", PROVIDER_REVISION_DIFF_MODEL_LIMIT)?;
    crate::providers::revisions::provider_revision_models_from_rows(
        pool,
        rows,
        Some(PROVIDER_REVISION_DIFF_CAPABILITY_LIMIT),
    )
    .await
}

pub async fn diff_provider_revisions(
    pool: &sqlx::PgPool,
    provider_id: Uuid,
    from_id: Uuid,
    to_id: Uuid,
) -> Result<ProviderRevisionDiff, Error> {
    let from =
        crate::providers::revisions::get_provider_revision(pool, provider_id, from_id).await?;
    let to = crate::providers::revisions::get_provider_revision(pool, provider_id, to_id).await?;
    for revision in [&from, &to] {
        enforce_provider_revision_diff_limit(
            usize::try_from(revision.model_count).unwrap_or(usize::MAX),
            "models",
            PROVIDER_REVISION_DIFF_MODEL_LIMIT,
        )?;
        enforce_provider_revision_diff_limit(
            usize::try_from(revision.capability_count).unwrap_or(usize::MAX),
            "capability tuples",
            PROVIDER_REVISION_DIFF_CAPABILITY_LIMIT,
        )?;
    }
    let from_model_records =
        crate::providers::revisions::all_provider_revision_models(pool, from_id).await?;
    let to_model_records =
        crate::providers::revisions::all_provider_revision_models(pool, to_id).await?;
    let from_models = provider_revision_model_map(&from_model_records);
    let to_models = provider_revision_model_map(&to_model_records);
    let from_capabilities = provider_revision_capability_set(&from_model_records);
    let to_capabilities = provider_revision_capability_set(&to_model_records);
    Ok(ProviderRevisionDiff {
        from_revision: from.revision,
        to_revision: to.revision,
        name_changed: from.name != to.name,
        endpoint_changed: from.configuration.endpoint != to.configuration.endpoint,
        cloud_context_changed: from.configuration.cloud_region != to.configuration.cloud_region
            || from.configuration.cloud_project != to.configuration.cloud_project,
        deployment_changed: from.configuration.deployment != to.configuration.deployment,
        api_version_changed: from.configuration.api_version != to.configuration.api_version,
        connector_changed: from.configuration.kind != to.configuration.kind
            || from.configuration.auth_mode != to.configuration.auth_mode
            || from.connector_ready != to.connector_ready,
        credential_changed: from.credential_version_id != to.credential_version_id,
        models_added: to_models
            .keys()
            .filter(|model| !from_models.contains_key(*model))
            .cloned()
            .collect(),
        models_removed: from_models
            .keys()
            .filter(|model| !to_models.contains_key(*model))
            .cloned()
            .collect(),
        models_changed: to_models
            .iter()
            .filter_map(|(model, state)| {
                from_models
                    .get(model)
                    .filter(|previous| *previous != state)
                    .map(|_| model.clone())
            })
            .collect(),
        capabilities_added: to_capabilities
            .difference(&from_capabilities)
            .cloned()
            .collect(),
        capabilities_removed: from_capabilities
            .difference(&to_capabilities)
            .cloned()
            .collect(),
    })
}

/// Restores only non-secret provider configuration and declared capability
/// tuples. The provider's currently selected, non-revoked credential is
/// preserved; the historical revision credential is never selected.
pub async fn restore_provider_revision_as_draft(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    provider_id: Uuid,
    revision_id: Uuid,
    expected_etag: Uuid,
    actor: Uuid,
    idempotency_key: &str,
) -> Result<ProviderRecord, Error> {
    let revision =
        crate::providers::revisions::get_provider_revision(pool, provider_id, revision_id).await?;
    let mut transaction = pool.begin().await?;
    if !claim_idempotency(
        &mut transaction,
        actor,
        "provider_revision.restore_as_draft",
        idempotency_key,
    )
    .await?
    {
        return Err(Error::IdempotencyConflict);
    }
    let provider = lock_provider(&mut transaction, provider_id)
        .await?
        .ok_or(Error::NotFound)?;
    if provider.etag != expected_etag {
        return Err(Error::PreconditionFailed);
    }
    if provider.configuration.kind != revision.configuration.kind {
        return Err(Error::Invalid(
            "a historical revision cannot change the provider connector kind".to_owned(),
        ));
    }
    let selected_credential: Option<Uuid> = provider.active_credential_version_id;
    let selected_credential = if let Some(credential_id) = selected_credential {
        sqlx::query_scalar::<_, uuid::Uuid>(
            "SELECT id FROM provider_credential_versions \
                 WHERE id = $1 AND provider_id = $2 AND revoked_at IS NULL",
        )
        .bind(credential_id)
        .bind(provider_id)
        .fetch_optional(&mut *transaction)
        .await?
    } else {
        None
    };
    let etag = Uuid::now_v7();
    sqlx::query(
        "UPDATE providers SET name = $1, endpoint = $2, cloud_region = $3, \
                    cloud_project = $4, deployment = $5, api_version = $6, auth_mode = $7, \
                    connector_ready = $8, active_credential_version_id = $9, \
                    state = 'draft'::provider_state, etag = $10, updated_at = now(), \
                    last_probe_at = NULL, last_probe_status = NULL, last_probe_detail = NULL \
             WHERE id = $11",
    )
    .bind(&revision.name)
    .bind(revision.configuration.endpoint.as_deref())
    .bind(revision.configuration.cloud_region.as_deref())
    .bind(revision.configuration.cloud_project.as_deref())
    .bind(revision.configuration.deployment.as_deref())
    .bind(revision.configuration.api_version.as_deref())
    .bind(revision.configuration.auth_mode.as_str())
    .bind(revision.connector_ready)
    .bind(selected_credential)
    .bind(etag)
    .bind(provider_id)
    .execute(&mut *transaction)
    .await?;
    restore_revision_models(&mut transaction, provider_id, revision_id).await?;
    record_success(
        &mut *transaction,
        provenance,
        actor,
        "provider_revision.restore_as_draft",
        "provider",
        provider_id,
    )
    .await?;
    complete_idempotency(
        &mut transaction,
        actor,
        "provider_revision.restore_as_draft",
        idempotency_key,
        &provider_id.to_string(),
    )
    .await?;
    transaction.commit().await?;
    let restored = crate::providers::repository::get_provider(pool, provider_id).await?;
    debug_assert_eq!(restored.etag, etag);
    Ok(restored)
}

#[derive(Debug, sqlx::FromRow)]
struct ProviderRevisionRow {
    id: Uuid,
    provider_id: Uuid,
    revision: i32,
    name: String,
    #[sqlx(flatten)]
    configuration: crate::providers::configuration::ProviderConfiguration,

    connector_ready: bool,
    credential_version_id: Option<Uuid>,
    credential_version: Option<i32>,
    source_etag: Uuid,
    activated_by: Uuid,
    activated_at: DateTime<Utc>,
    model_count: i64,
    enabled_model_count: i64,
    capability_count: i64,
    certified_capability_count: i64,
}

#[derive(Debug, sqlx::FromRow)]
struct ProviderRevisionModelRow {
    revision_model_id: Uuid,
    source_provider_model_id: Uuid,
    upstream_model: String,
    display_name: String,
    enabled: bool,
    discovered_at: Option<DateTime<Utc>>,
}

#[derive(Debug, sqlx::FromRow)]
struct RevisionCapabilityRow {
    provider_revision_model_id: Uuid,
    operation: String,
    surface: String,
    mode: String,
    source: String,
    certified_at: Option<DateTime<Utc>>,
}

impl RevisionCapabilityRow {
    fn split(self) -> (Uuid, CapabilityRow) {
        (
            self.provider_revision_model_id,
            CapabilityRow {
                operation: self.operation,
                surface: self.surface,
                mode: self.mode,
                source: self.source,
                certified_at: self.certified_at,
            },
        )
    }
}

fn provider_revision_from_row(row: ProviderRevisionRow) -> Result<ProviderRevisionRecord, Error> {
    Ok(ProviderRevisionRecord {
        id: row.id,
        provider_id: row.provider_id,
        revision: row.revision,
        name: row.name,
        configuration: row.configuration,

        connector_ready: row.connector_ready,
        credential_version_id: row.credential_version_id,
        credential_version: row.credential_version,
        source_etag: row.source_etag,
        activated_by: row.activated_by,
        activated_at: row.activated_at,
        model_count: checked_configuration_count(row.model_count, "model_count")?,
        enabled_model_count: checked_configuration_count(
            row.enabled_model_count,
            "enabled_model_count",
        )?,
        capability_count: checked_configuration_count(row.capability_count, "capability_count")?,
        certified_capability_count: checked_configuration_count(
            row.certified_capability_count,
            "certified_capability_count",
        )?,
    })
}

async fn ensure_provider_revision_exists(
    pool: &PgPool,
    provider_id: Uuid,
    revision_id: Uuid,
) -> Result<(), Error> {
    let exists: bool = sqlx::query_scalar::<_, bool>(
        "SELECT EXISTS (SELECT 1 FROM provider_revisions \
         WHERE provider_id = $1 AND id = $2) AS \"value\"",
    )
    .bind(provider_id)
    .bind(revision_id)
    .fetch_one(pool)
    .await?;
    exists.then_some(()).ok_or(Error::NotFound)
}

fn provider_revision_model_map(
    models: &[ProviderModelRecord],
) -> BTreeMap<String, (String, bool, Option<DateTime<Utc>>)> {
    models
        .iter()
        .map(|model| {
            (
                model.upstream_model.clone(),
                (
                    model.display_name.clone(),
                    model.enabled,
                    model.discovered_at,
                ),
            )
        })
        .collect()
}

fn provider_revision_capability_set(models: &[ProviderModelRecord]) -> BTreeSet<String> {
    models
        .iter()
        .flat_map(|model| {
            model.capabilities.iter().map(move |capability| {
                format!(
                    "{}/{}/{}/{}",
                    model.upstream_model, capability.operation, capability.surface, capability.mode
                )
            })
        })
        .collect()
}

async fn restore_revision_models(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    provider_id: Uuid,
    revision_id: Uuid,
) -> Result<(), Error> {
    sqlx::query("UPDATE provider_models SET enabled = false WHERE provider_id = $1")
        .bind(provider_id)
        .execute(&mut **transaction)
        .await?;
    sqlx::query(
        "UPDATE provider_models pm SET upstream_model = prm.upstream_model, \
                    display_name = prm.display_name, enabled = prm.enabled, \
                    discovered_at = prm.discovered_at \
             FROM provider_revision_models prm \
             WHERE prm.provider_revision_id = $1 \
               AND pm.id = prm.source_provider_model_id AND pm.provider_id = $2",
    )
    .bind(revision_id)
    .bind(provider_id)
    .execute(&mut **transaction)
    .await?;
    sqlx::query(
        "DELETE FROM model_capabilities WHERE provider_model_id IN \
               (SELECT id FROM provider_models WHERE provider_id = $1)",
    )
    .bind(provider_id)
    .execute(&mut **transaction)
    .await?;
    sqlx::query(
        "INSERT INTO model_capabilities \
               (provider_model_id, operation, surface, mode, source, certified_at) \
             SELECT prm.source_provider_model_id, prc.operation, prc.surface, prc.mode, \
                    'declared', NULL \
             FROM provider_revision_models prm \
             JOIN provider_revision_capabilities prc \
               ON prc.provider_revision_model_id = prm.id \
             WHERE prm.provider_revision_id = $1",
    )
    .bind(revision_id)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}
