use super::{
    helpers::{
        CapabilityRow, audit_in_transaction, capability_from_row, checked_configuration_count,
    },
    *,
};

impl PgStore {
    pub async fn list_provider_revisions(
        &self,
        provider_id: Uuid,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<ConfigurationPage<ProviderRevisionRecord>, ConfigurationError> {
        let limit = checked_limit(limit)?;
        let exists: bool = sqlx::query_scalar!(
            "SELECT EXISTS (SELECT 1 FROM providers WHERE id = $1) AS \"value!\"",
            provider_id
        )
        .fetch_one(self.pool())
        .await?;
        if !exists {
            return Err(ConfigurationError::NotFound);
        }
        let before_revision: Option<i32> = match cursor {
            Some(cursor) => Some(
                sqlx::query_scalar!(
                    "SELECT revision FROM provider_revisions WHERE provider_id = $1 AND id = $2",
                    provider_id,
                    cursor
                )
                .fetch_optional(self.pool())
                .await?
                .ok_or_else(|| {
                    ConfigurationError::Invalid(
                        "provider-revision pagination cursor is invalid".to_owned(),
                    )
                })?,
            ),
            None => None,
        };
        let rows = sqlx::query_as!(
            ProviderRevisionRow,
            "SELECT pr.id, pr.provider_id, pr.revision, pr.name, pr.kind, pr.endpoint, \
                    pr.cloud_region, pr.cloud_project, pr.deployment, pr.api_version, \
                    pr.auth_mode, pr.connector_ready, pr.credential_version_id, \
                    cv.version AS \"credential_version?\", pr.source_etag, pr.activated_by, \
                    pr.activated_at, stats.model_count AS \"model_count!\", \
                    stats.enabled_model_count AS \"enabled_model_count!\", \
                    stats.capability_count AS \"capability_count!\", \
                    stats.certified_capability_count AS \"certified_capability_count!\" \
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
            provider_id,
            before_revision,
            limit + 1
        )
        .fetch_all(self.pool())
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
        &self,
        provider_id: Uuid,
        revision_id: Uuid,
    ) -> Result<ProviderRevisionRecord, ConfigurationError> {
        let row = sqlx::query_as!(
            ProviderRevisionRow,
            "SELECT pr.id, pr.provider_id, pr.revision, pr.name, pr.kind, pr.endpoint, \
                    pr.cloud_region, pr.cloud_project, pr.deployment, pr.api_version, \
                    pr.auth_mode, pr.connector_ready, pr.credential_version_id, \
                    cv.version AS \"credential_version?\", pr.source_etag, pr.activated_by, \
                    pr.activated_at, stats.model_count AS \"model_count!\", \
                    stats.enabled_model_count AS \"enabled_model_count!\", \
                    stats.capability_count AS \"capability_count!\", \
                    stats.certified_capability_count AS \"certified_capability_count!\" \
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
            provider_id,
            revision_id
        )
        .fetch_optional(self.pool())
        .await?
        .ok_or(ConfigurationError::NotFound)?;
        provider_revision_from_row(row)
    }

    pub async fn list_provider_revision_models(
        &self,
        provider_id: Uuid,
        revision_id: Uuid,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<ConfigurationPage<ProviderModelRecord>, ConfigurationError> {
        let limit = checked_limit(limit)?;
        ensure_provider_revision_exists(self, provider_id, revision_id).await?;
        let rows = sqlx::query_as!(
            ProviderRevisionModelRow,
            "SELECT id AS revision_model_id, source_provider_model_id, upstream_model, \
                    display_name, enabled, discovered_at \
             FROM provider_revision_models WHERE provider_revision_id = $1 \
               AND ($2::uuid IS NULL OR id > $2) ORDER BY id LIMIT $3",
            revision_id,
            cursor,
            limit + 1
        )
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.revision_model_id);
        let items = self.provider_revision_models_from_rows(rows, None).await?;
        Ok(ConfigurationPage { items, next_cursor })
    }

    async fn provider_revision_models_from_rows(
        &self,
        rows: Vec<ProviderRevisionModelRow>,
        capability_limit: Option<usize>,
    ) -> Result<Vec<ProviderModelRecord>, ConfigurationError> {
        let revision_model_ids = rows
            .iter()
            .map(|row| row.revision_model_id)
            .collect::<Vec<_>>();
        let capability_rows =
            if revision_model_ids.is_empty() {
                Vec::new()
            } else if let Some(limit) = capability_limit {
                sqlx::query_as!(
                RevisionCapabilityRow,
                "SELECT provider_revision_model_id, operation, surface, mode, source, certified_at \
                 FROM provider_revision_capabilities \
                 WHERE provider_revision_model_id = ANY($1::uuid[]) \
                 ORDER BY provider_revision_model_id, operation, surface, mode LIMIT $2",
            &revision_model_ids, limit as i64 + 1)
            .fetch_all(self.pool())
            .await?
            } else {
                sqlx::query_as!(
                RevisionCapabilityRow,
                "SELECT provider_revision_model_id, operation, surface, mode, source, certified_at \
                 FROM provider_revision_capabilities \
                 WHERE provider_revision_model_id = ANY($1::uuid[]) \
                 ORDER BY provider_revision_model_id, operation, surface, mode",
            &revision_model_ids)
            .fetch_all(self.pool())
            .await?
            };
        if let Some(limit) = capability_limit {
            enforce_provider_revision_diff_limit(
                capability_rows.len(),
                "capability tuples",
                limit,
            )?;
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

    async fn all_provider_revision_models(
        &self,
        revision_id: Uuid,
    ) -> Result<Vec<ProviderModelRecord>, ConfigurationError> {
        let rows = sqlx::query_as!(
            ProviderRevisionModelRow,
            "SELECT id AS revision_model_id, source_provider_model_id, upstream_model, \
                    display_name, enabled, discovered_at \
             FROM provider_revision_models WHERE provider_revision_id = $1 ORDER BY id LIMIT $2",
            revision_id,
            PROVIDER_REVISION_DIFF_MODEL_LIMIT as i64 + 1
        )
        .fetch_all(self.pool())
        .await?;
        enforce_provider_revision_diff_limit(
            rows.len(),
            "models",
            PROVIDER_REVISION_DIFF_MODEL_LIMIT,
        )?;
        self.provider_revision_models_from_rows(rows, Some(PROVIDER_REVISION_DIFF_CAPABILITY_LIMIT))
            .await
    }

    pub async fn diff_provider_revisions(
        &self,
        provider_id: Uuid,
        from_id: Uuid,
        to_id: Uuid,
    ) -> Result<ProviderRevisionDiff, ConfigurationError> {
        let from = self.get_provider_revision(provider_id, from_id).await?;
        let to = self.get_provider_revision(provider_id, to_id).await?;
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
        let from_model_records = self.all_provider_revision_models(from_id).await?;
        let to_model_records = self.all_provider_revision_models(to_id).await?;
        let from_models = provider_revision_model_map(&from_model_records);
        let to_models = provider_revision_model_map(&to_model_records);
        let from_capabilities = provider_revision_capability_set(&from_model_records);
        let to_capabilities = provider_revision_capability_set(&to_model_records);
        Ok(ProviderRevisionDiff {
            from_revision: from.revision,
            to_revision: to.revision,
            name_changed: from.name != to.name,
            endpoint_changed: from.endpoint != to.endpoint,
            cloud_context_changed: from.cloud_region != to.cloud_region
                || from.cloud_project != to.cloud_project,
            deployment_changed: from.deployment != to.deployment,
            api_version_changed: from.api_version != to.api_version,
            connector_changed: from.kind != to.kind
                || from.auth_mode != to.auth_mode
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
        &self,
        provider_id: Uuid,
        revision_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<ProviderRecord, ConfigurationError> {
        let revision = self.get_provider_revision(provider_id, revision_id).await?;
        let mut transaction = self.pool().begin().await?;
        if !claim_idempotency(
            &mut transaction,
            actor,
            "provider_revision.restore_as_draft",
            idempotency_key,
        )
        .await?
        {
            return Err(ConfigurationError::IdempotencyConflict);
        }
        let provider = sqlx::query!(
            "SELECT etag, kind, active_credential_version_id \
             FROM providers WHERE id = $1 FOR UPDATE",
            provider_id
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(ConfigurationError::NotFound)?;
        if provider.etag != expected_etag {
            return Err(ConfigurationError::PreconditionFailed);
        }
        if provider.kind != revision.kind.as_str() {
            return Err(ConfigurationError::Invalid(
                "a historical revision cannot change the provider connector kind".to_owned(),
            ));
        }
        let selected_credential: Option<Uuid> = provider.active_credential_version_id;
        let selected_credential = if let Some(credential_id) = selected_credential {
            sqlx::query_scalar!(
                "SELECT id FROM provider_credential_versions \
                 WHERE id = $1 AND provider_id = $2 AND revoked_at IS NULL",
                credential_id,
                provider_id
            )
            .fetch_optional(&mut *transaction)
            .await?
        } else {
            None
        };
        let etag = Uuid::now_v7();
        sqlx::query!(
            "UPDATE providers SET name = $1, endpoint = $2, cloud_region = $3, \
                    cloud_project = $4, deployment = $5, api_version = $6, auth_mode = $7, \
                    connector_ready = $8, active_credential_version_id = $9, \
                    state = 'draft'::provider_state, etag = $10, updated_at = now(), \
                    last_probe_at = NULL, last_probe_status = NULL, last_probe_detail = NULL \
             WHERE id = $11",
            &revision.name,
            revision.endpoint.as_deref(),
            revision.cloud_region.as_deref(),
            revision.cloud_project.as_deref(),
            revision.deployment.as_deref(),
            revision.api_version.as_deref(),
            revision.auth_mode.as_str(),
            revision.connector_ready,
            selected_credential,
            etag,
            provider_id
        )
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "UPDATE provider_models SET enabled = false WHERE provider_id = $1",
            provider_id
        )
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "UPDATE provider_models pm SET upstream_model = prm.upstream_model, \
                    display_name = prm.display_name, enabled = prm.enabled, \
                    discovered_at = prm.discovered_at \
             FROM provider_revision_models prm \
             WHERE prm.provider_revision_id = $1 \
               AND pm.id = prm.source_provider_model_id AND pm.provider_id = $2",
            revision_id,
            provider_id
        )
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "DELETE FROM model_capabilities WHERE provider_model_id IN \
               (SELECT id FROM provider_models WHERE provider_id = $1)",
            provider_id
        )
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "INSERT INTO model_capabilities \
               (provider_model_id, operation, surface, mode, source, certified_at) \
             SELECT prm.source_provider_model_id, prc.operation, prc.surface, prc.mode, \
                    'declared', NULL \
             FROM provider_revision_models prm \
             JOIN provider_revision_capabilities prc \
               ON prc.provider_revision_model_id = prm.id \
             WHERE prm.provider_revision_id = $1",
            revision_id
        )
        .execute(&mut *transaction)
        .await?;
        audit_in_transaction(
            &mut transaction,
            actor,
            "provider_revision.restore_as_draft",
            "provider",
            provider_id,
            "success",
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
        let restored = self.get_provider(provider_id).await?;
        debug_assert_eq!(restored.etag, etag);
        Ok(restored)
    }
}

#[derive(Debug, sqlx::FromRow)]
struct ProviderRevisionRow {
    id: Uuid,
    provider_id: Uuid,
    revision: i32,
    name: String,
    kind: String,
    endpoint: Option<String>,
    cloud_region: Option<String>,
    cloud_project: Option<String>,
    deployment: Option<String>,
    api_version: Option<String>,
    auth_mode: String,
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

fn provider_revision_from_row(
    row: ProviderRevisionRow,
) -> Result<ProviderRevisionRecord, ConfigurationError> {
    Ok(ProviderRevisionRecord {
        id: row.id,
        provider_id: row.provider_id,
        revision: row.revision,
        name: row.name,
        kind: row
            .kind
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("provider revision kind"))?,
        endpoint: row.endpoint,
        cloud_region: row.cloud_region,
        cloud_project: row.cloud_project,
        deployment: row.deployment,
        api_version: row.api_version,
        auth_mode: row.auth_mode.parse().map_err(|_| {
            PersistenceError::InvalidStoredValue("provider revision authentication mode")
        })?,
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
    store: &PgStore,
    provider_id: Uuid,
    revision_id: Uuid,
) -> Result<(), ConfigurationError> {
    let exists: bool = sqlx::query_scalar!(
        "SELECT EXISTS (SELECT 1 FROM provider_revisions \
         WHERE provider_id = $1 AND id = $2) AS \"value!\"",
        provider_id,
        revision_id
    )
    .fetch_one(store.pool())
    .await?;
    exists.then_some(()).ok_or(ConfigurationError::NotFound)
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
