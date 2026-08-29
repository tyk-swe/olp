use super::{
    helpers::{checked_configuration_count, lock_provider},
    *,
};
use crate::audit_events::{AuditEvent, record_audit_event, record_success};
use crate::configuration::validation::transport_changed;

impl Store {
    pub async fn list_providers(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<ConfigurationPage<ProviderRecord>, Error> {
        let limit = checked_limit(limit)?;
        let rows = sqlx::query!(
            "SELECT id FROM providers WHERE ($1::uuid IS NULL OR id > $1) ORDER BY id LIMIT $2",
            cursor,
            limit + 1
        )
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.id);
        let ids: Vec<Uuid> = rows.into_iter().map(|row| row.id).collect();
        let items = self.get_providers(&ids).await?;
        Ok(ConfigurationPage { items, next_cursor })
    }

    /// Reads providers in the order of `ids`; the one projection every
    /// provider read shares lives here. Any missing id is `NotFound`.
    pub async fn get_providers(&self, ids: &[Uuid]) -> Result<Vec<ProviderRecord>, Error> {
        if ids.is_empty() {
            return Ok(Vec::new());
        }
        let rows = sqlx::query_as!(
            ProviderRow,
            "SELECT p.id AS \"id!\", p.name AS \"name!\", p.kind AS \"kind!\", \
                    p.state::text AS \"state!\", p.endpoint, p.cloud_region, \
                    p.cloud_project, p.deployment, p.api_version, p.auth_mode AS \"auth_mode!\", \
                    p.connector_ready AS \"connector_ready!\", \
                    p.etag AS \"etag!\", ar.revision AS \"active_revision?\", \
                    (p.state = 'draft'::provider_state AND p.active_revision_id IS NOT NULL) \
                      AS \"pending_activation!\", \
                    p.active_credential_version_id AS draft_credential_id, \
                    draft_cv.version AS \"draft_credential_version?\", \
                    ar.credential_version_id AS \"runtime_credential_id?\", \
                    runtime_cv.version AS \"runtime_credential_version?\", \
                    p.last_probe_at, p.last_probe_status, p.last_probe_detail, \
                    p.created_at AS \"created_at!\", p.updated_at AS \"updated_at!\", \
                    stats.model_count AS \"model_count!\", \
                    stats.enabled_model_count AS \"enabled_model_count!\", \
                    stats.capability_count AS \"capability_count!\", \
                    stats.certified_capability_count AS \"certified_capability_count!\", \
                    probe.upstream_model AS \"probe_model?\", \
                    creator.email AS \"created_by_email?\" \
             FROM providers p \
             LEFT JOIN users creator ON creator.id = p.created_by \
             LEFT JOIN provider_credential_versions draft_cv \
               ON draft_cv.id = p.active_credential_version_id \
             LEFT JOIN provider_revisions ar ON ar.id = p.active_revision_id \
             LEFT JOIN provider_credential_versions runtime_cv \
               ON runtime_cv.id = ar.credential_version_id \
             LEFT JOIN LATERAL ( \
                 SELECT COUNT(DISTINCT pm.id)::bigint AS model_count, \
                        COUNT(DISTINCT pm.id) FILTER (WHERE pm.enabled)::bigint \
                          AS enabled_model_count, \
                        COUNT(mc.provider_model_id)::bigint AS capability_count, \
                        COUNT(mc.provider_model_id) FILTER (WHERE mc.source = 'certified')::bigint \
                          AS certified_capability_count \
                 FROM provider_models pm \
                 LEFT JOIN model_capabilities mc ON mc.provider_model_id = pm.id \
                 WHERE pm.provider_id = p.id \
             ) stats ON true \
             LEFT JOIN LATERAL ( \
                 SELECT pm.upstream_model FROM provider_models pm \
                 WHERE pm.provider_id = p.id ORDER BY pm.id LIMIT 1 \
             ) probe ON true \
             WHERE p.id = ANY($1::uuid[])",
            ids
        )
        .fetch_all(self.pool())
        .await?;
        let mut by_id: BTreeMap<Uuid, ProviderRow> =
            rows.into_iter().map(|row| (row.id, row)).collect();
        ids.iter()
            .map(|id| by_id.remove(id).ok_or(Error::NotFound))
            .map(|row| row.and_then(provider_from_row))
            .collect()
    }

    pub async fn get_provider(&self, provider_id: Uuid) -> Result<ProviderRecord, Error> {
        self.get_providers(&[provider_id])
            .await?
            .into_iter()
            .next()
            .ok_or(Error::NotFound)
    }

    pub async fn update_provider(
        &self,
        provider_id: Uuid,
        expected_etag: Uuid,
        update: &UpdateProvider,
        actor: Uuid,
    ) -> Result<Uuid, Error> {
        validate_provider_update(update)?;
        let etag = Uuid::now_v7();
        let mut transaction = self.pool().begin().await?;
        let current = lock_provider(&mut transaction, provider_id)
            .await?
            .ok_or(Error::NotFound)?;
        if current.etag != expected_etag {
            return Err(Error::PreconditionFailed);
        }
        if current.state == "disabled" {
            return Err(Error::InUse);
        }
        if transport_changed(&current, update) {
            replace_provider_transport(&mut transaction, provider_id, update, etag).await?;
        } else {
            sqlx::query!(
                "UPDATE providers SET name = $1, state = 'draft'::provider_state, etag = $2 \
                 WHERE id = $3",
                update.name.trim(),
                etag,
                provider_id
            )
            .execute(&mut *transaction)
            .await?;
        }
        record_success(
            &mut *transaction,
            self.provenance(),
            actor,
            "provider.update",
            "provider",
            provider_id,
        )
        .await?;
        transaction.commit().await?;
        Ok(etag)
    }

    pub async fn disable_provider(
        &self,
        provider_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<ProviderMutationResult, Error> {
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        prepare_runtime_mutation(&mut transaction).await?;
        if !claim_idempotency(&mut transaction, actor, "provider.disable", idempotency_key).await? {
            return Err(Error::IdempotencyConflict);
        }
        // Serialize against the short reservation INSERT so the decision and
        // runtime publication cannot race a newly committed upstream job.
        sqlx::query!("LOCK TABLE async_media_jobs IN SHARE MODE")
            .execute(&mut *transaction)
            .await?;
        let has_live_media_jobs: bool = sqlx::query_scalar!(
            "SELECT EXISTS (SELECT 1 FROM async_media_jobs
             WHERE provider_id = $1 AND lifecycle_state <> 'deleted') AS \"value!\"",
            provider_id
        )
        .fetch_one(&mut *transaction)
        .await?;
        if has_live_media_jobs {
            return Err(Error::InUse);
        }
        let referenced: bool = sqlx::query_scalar!(
            "SELECT EXISTS ( \
               SELECT 1 FROM routes r \
               JOIN LATERAL (SELECT id FROM route_revisions WHERE route_id = r.id \
                             ORDER BY revision DESC LIMIT 1) rr ON true \
               JOIN route_revision_targets rt ON rt.route_revision_id = rr.id \
               JOIN provider_models pm ON pm.id = rt.provider_model_id \
               WHERE pm.provider_id = $1 \
             ) AS \"value!\"",
            provider_id
        )
        .fetch_one(&mut *transaction)
        .await?;
        if referenced {
            return Err(Error::InUse);
        }
        let etag = Uuid::now_v7();
        let updated = sqlx::query!(
            "UPDATE providers SET state = 'disabled'::provider_state, active_revision_id = NULL, \
                    etag = $1, updated_at = now() \
             WHERE id = $2 AND etag = $3 AND state <> 'disabled'::provider_state \
               AND active_revision_id IS NOT NULL",
            etag,
            provider_id,
            expected_etag
        )
        .execute(&mut *transaction)
        .await?;
        if updated.rows_affected() != 1 {
            let row = sqlx::query!("SELECT etag FROM providers WHERE id = $1", provider_id)
                .fetch_optional(&mut *transaction)
                .await?
                .ok_or(Error::NotFound)?;
            return Err(if row.etag != expected_etag {
                Error::PreconditionFailed
            } else {
                Error::InUse
            });
        }
        record_success(
            &mut *transaction,
            self.provenance(),
            actor,
            "provider.disable",
            "provider",
            provider_id,
        )
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "provider.disable",
            idempotency_key,
            &provider_id.to_string(),
        )
        .await?;
        let release = compile_and_publish_runtime_in_transaction(&mut transaction, actor).await?;
        transaction.commit().await?;
        Ok(ProviderMutationResult {
            etag,
            release: Some(release),
        })
    }

    pub async fn restore_provider_as_draft(
        &self,
        provider_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<Uuid, Error> {
        let mut transaction = self.pool().begin().await?;
        if !claim_idempotency(
            &mut transaction,
            actor,
            "provider.restore_as_draft",
            idempotency_key,
        )
        .await?
        {
            return Err(Error::IdempotencyConflict);
        }
        let etag = Uuid::now_v7();
        let updated = sqlx::query!(
            "UPDATE providers SET state = 'draft'::provider_state, etag = $1, updated_at = now(), \
                    last_probe_at = NULL, last_probe_status = NULL, last_probe_detail = NULL \
             WHERE id = $2 AND etag = $3 AND state = 'disabled'::provider_state",
            etag,
            provider_id,
            expected_etag
        )
        .execute(&mut *transaction)
        .await?;
        if updated.rows_affected() != 1 {
            let row = sqlx::query!("SELECT etag FROM providers WHERE id = $1", provider_id)
                .fetch_optional(&mut *transaction)
                .await?
                .ok_or(Error::NotFound)?;
            return Err(if row.etag != expected_etag {
                Error::PreconditionFailed
            } else {
                Error::InUse
            });
        }
        sqlx::query!(
            "UPDATE model_capabilities SET source = 'declared', certified_at = NULL \
             WHERE provider_model_id IN (SELECT id FROM provider_models WHERE provider_id = $1)",
            provider_id
        )
        .execute(&mut *transaction)
        .await?;
        record_success(
            &mut *transaction,
            self.provenance(),
            actor,
            "provider.restore_as_draft",
            "provider",
            provider_id,
        )
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "provider.restore_as_draft",
            idempotency_key,
            &provider_id.to_string(),
        )
        .await?;
        transaction.commit().await?;
        Ok(etag)
    }

    pub async fn record_provider_probe(
        &self,
        provider_id: Uuid,
        expected_etag: Uuid,
        succeeded: bool,
        detail: &str,
        actor: Uuid,
    ) -> Result<DateTime<Utc>, Error> {
        let detail = detail.trim();
        if detail.chars().count() > 500 {
            return Err(Error::Invalid(
                "probe detail exceeds 500 characters".to_owned(),
            ));
        }
        let at = Utc::now();
        let mut transaction = self.pool().begin().await?;
        let result = sqlx::query!(
            "UPDATE providers SET last_probe_at = $1, last_probe_status = $2, \
                    last_probe_detail = $3 WHERE id = $4 AND etag = $5",
            at,
            if succeeded { "succeeded" } else { "failed" },
            detail,
            provider_id,
            expected_etag
        )
        .execute(&mut *transaction)
        .await?;
        if result.rows_affected() != 1 {
            let current_etag: Option<Uuid> =
                sqlx::query_scalar!("SELECT etag FROM providers WHERE id = $1", provider_id)
                    .fetch_optional(&mut *transaction)
                    .await?;
            return Err(if current_etag.is_some() {
                Error::PreconditionFailed
            } else {
                Error::NotFound
            });
        }
        record_audit_event(
            &mut *transaction,
            AuditEvent {
                provenance: self.provenance(),
                actor: Some(actor),
                action: "provider.probe",
                resource_type: "provider",
                resource_id: Some(&provider_id.to_string()),
                outcome: if succeeded { "success" } else { "failure" },
                occurred_at: None,
            },
        )
        .await?;
        transaction.commit().await?;
        Ok(at)
    }
}

#[derive(Debug, sqlx::FromRow)]
struct ProviderRow {
    id: Uuid,
    name: String,
    kind: String,
    state: String,
    endpoint: Option<String>,
    cloud_region: Option<String>,
    cloud_project: Option<String>,
    deployment: Option<String>,
    api_version: Option<String>,
    auth_mode: String,
    connector_ready: bool,
    etag: Uuid,
    active_revision: Option<i32>,
    pending_activation: bool,
    draft_credential_id: Option<Uuid>,
    draft_credential_version: Option<i32>,
    runtime_credential_id: Option<Uuid>,
    runtime_credential_version: Option<i32>,
    last_probe_at: Option<DateTime<Utc>>,
    last_probe_status: Option<String>,
    last_probe_detail: Option<String>,
    created_at: DateTime<Utc>,
    updated_at: DateTime<Utc>,
    model_count: i64,
    enabled_model_count: i64,
    capability_count: i64,
    certified_capability_count: i64,
    probe_model: Option<String>,
    created_by_email: Option<String>,
}

fn provider_from_row(row: ProviderRow) -> Result<ProviderRecord, Error> {
    let active_revision = row
        .active_revision
        .map(u32::try_from)
        .transpose()
        .map_err(|_| Error::Invalid("provider revision is invalid".to_owned()))?;
    Ok(ProviderRecord {
        id: row.id,
        name: row.name,
        kind: row
            .kind
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("provider kind"))?,
        state: row
            .state
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("provider state"))?,
        endpoint: row.endpoint,
        cloud_region: row.cloud_region,
        cloud_project: row.cloud_project,
        deployment: row.deployment,
        api_version: row.api_version,
        auth_mode: row
            .auth_mode
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("provider authentication mode"))?,
        connector_ready: row.connector_ready,
        etag: row.etag,
        active_revision,
        pending_activation: row.pending_activation,
        draft_credential_id: row.draft_credential_id,
        draft_credential_version: row.draft_credential_version,
        runtime_credential_id: row.runtime_credential_id,
        runtime_credential_version: row.runtime_credential_version,
        last_probe_at: row.last_probe_at,
        last_probe_status: row.last_probe_status,
        last_probe_detail: row.last_probe_detail,
        created_at: row.created_at,
        updated_at: row.updated_at,
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
        probe_model: row.probe_model,
        created_by_email: row.created_by_email,
    })
}

/// A transport edit invalidates every piece of evidence gathered against the
/// previous configuration: the probe columns and all capability certification.
async fn replace_provider_transport(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    provider_id: Uuid,
    update: &UpdateProvider,
    etag: Uuid,
) -> Result<(), Error> {
    sqlx::query!(
        "UPDATE providers SET name = $1, endpoint = $2, cloud_region = $3, cloud_project = $4, \
                deployment = $5, api_version = $6, auth_mode = $7, \
                active_credential_version_id = CASE \
                  WHEN $7 IN ('adc', 'default_chain') THEN NULL \
                  ELSE active_credential_version_id END, \
                state = 'draft'::provider_state, etag = $8, updated_at = now(), \
                last_probe_at = NULL, last_probe_status = NULL, last_probe_detail = NULL \
         WHERE id = $9",
        update.name.trim(),
        update.endpoint.as_deref().map(str::trim),
        update.cloud_region.as_deref().map(str::trim),
        update.cloud_project.as_deref().map(str::trim),
        update.deployment.as_deref().map(str::trim),
        update.api_version.as_deref().map(str::trim),
        update.auth_mode.as_str(),
        etag,
        provider_id
    )
    .execute(&mut **transaction)
    .await?;
    sqlx::query!(
        "UPDATE model_capabilities SET source = 'declared', certified_at = NULL \
         WHERE provider_model_id IN \
           (SELECT id FROM provider_models WHERE provider_id = $1)",
        provider_id
    )
    .execute(&mut **transaction)
    .await?;
    Ok(())
}
