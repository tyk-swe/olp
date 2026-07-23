use super::{
    helpers::{CapabilityRow, audit_in_transaction, capability_from_row},
    *,
};

impl PgStore {
    pub async fn list_provider_models(
        &self,
        provider_id: Uuid,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<ConfigurationPage<ProviderModelRecord>, ConfigurationError> {
        let limit = checked_limit(limit)?;
        ensure_provider_exists(self, provider_id).await?;
        let rows = sqlx::query_as!(
            ProviderModelRow,
            "SELECT id, upstream_model, display_name, enabled, discovered_at \
             FROM provider_models WHERE provider_id = $1 \
               AND ($2::uuid IS NULL OR id > $2) ORDER BY id LIMIT $3",
            provider_id,
            cursor,
            limit + 1
        )
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.id);
        let items = self.provider_models_from_rows(rows).await?;
        Ok(ConfigurationPage { items, next_cursor })
    }

    pub async fn list_provider_model_inventory(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
        enabled: Option<bool>,
    ) -> Result<ConfigurationPage<ProviderModelInventoryRecord>, ConfigurationError> {
        let limit = checked_limit(limit)?;
        let rows = sqlx::query_as!(
            ProviderInventoryRow,
            "SELECT pm.id, pm.upstream_model, pm.display_name, pm.enabled, pm.discovered_at, \
                    p.id AS provider_id, p.name AS provider_name, p.kind AS provider_kind \
             FROM provider_models pm JOIN providers p ON p.id = pm.provider_id \
             WHERE ($1::uuid IS NULL OR pm.id > $1) \
               AND ($2::boolean IS NULL OR pm.enabled = $2) \
             ORDER BY pm.id LIMIT $3",
            cursor,
            enabled,
            limit + 1
        )
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.id);
        let mut providers = rows
            .iter()
            .map(|row| {
                Ok((
                    row.id,
                    (
                        row.provider_id,
                        row.provider_name.clone(),
                        row.provider_kind
                            .parse()
                            .map_err(|_| PersistenceError::InvalidStoredValue("provider kind"))?,
                    ),
                ))
            })
            .collect::<Result<BTreeMap<_, _>, ConfigurationError>>()?;
        let model_rows = rows.into_iter().map(ProviderInventoryRow::model).collect();
        let items = self
            .provider_models_from_rows(model_rows)
            .await?
            .into_iter()
            .map(|model| {
                let (provider_id, provider_name, provider_kind) = providers
                    .remove(&model.id)
                    .ok_or(PersistenceError::InvalidStoredValue(
                        "provider metadata for model",
                    ))?;
                Ok(ProviderModelInventoryRecord {
                    provider_id,
                    provider_name,
                    provider_kind,
                    model,
                })
            })
            .collect::<Result<Vec<_>, ConfigurationError>>()?;
        Ok(ConfigurationPage { items, next_cursor })
    }

    pub async fn get_provider_model(
        &self,
        provider_id: Uuid,
        model_id: Uuid,
    ) -> Result<ProviderModelRecord, ConfigurationError> {
        let rows = sqlx::query_as!(
            ProviderModelRow,
            "SELECT id, upstream_model, display_name, enabled, discovered_at \
             FROM provider_models WHERE provider_id = $1 AND id = $2",
            provider_id,
            model_id
        )
        .fetch_all(self.pool())
        .await?;
        self.provider_models_from_rows(rows)
            .await?
            .into_iter()
            .next()
            .ok_or(ConfigurationError::NotFound)
    }

    async fn provider_models_from_rows(
        &self,
        rows: Vec<ProviderModelRow>,
    ) -> Result<Vec<ProviderModelRecord>, ConfigurationError> {
        let model_ids = rows.iter().map(|row| row.id).collect::<Vec<_>>();
        let capability_rows = if model_ids.is_empty() {
            Vec::new()
        } else {
            sqlx::query_as!(
                ModelCapabilityRow,
                "SELECT provider_model_id, operation, surface, mode, source, certified_at \
                 FROM model_capabilities WHERE provider_model_id = ANY($1::uuid[]) \
                 ORDER BY provider_model_id, operation, surface, mode",
                &model_ids
            )
            .fetch_all(self.pool())
            .await?
        };
        let mut capabilities = BTreeMap::<Uuid, Vec<CapabilityRecord>>::new();
        for row in capability_rows {
            let (provider_model_id, capability) = row.split();
            capabilities
                .entry(provider_model_id)
                .or_default()
                .push(capability_from_row(capability)?);
        }
        Ok(rows
            .into_iter()
            .map(|row| {
                let id = row.id;
                ProviderModelRecord {
                    id,
                    upstream_model: row.upstream_model,
                    display_name: row.display_name,
                    enabled: row.enabled,
                    discovered_at: row.discovered_at,
                    capabilities: capabilities.remove(&id).unwrap_or_default(),
                }
            })
            .collect())
    }

    pub async fn discover_provider_models(
        &self,
        provider_id: Uuid,
        expected_etag: Uuid,
        models: &[DiscoveredModelInput],
        actor: Uuid,
    ) -> Result<Uuid, ConfigurationError> {
        if models.is_empty() {
            return Err(ConfigurationError::Invalid(
                "discovery returned no models".to_owned(),
            ));
        }
        let mut names = BTreeSet::new();
        for model in models {
            validate_model(model)?;
            if !names.insert(model.upstream_model.trim()) {
                return Err(ConfigurationError::Invalid(
                    "model names must be unique".to_owned(),
                ));
            }
        }
        let mut transaction = self.pool().begin().await?;
        let provider = sqlx::query!(
            "SELECT etag, state::text AS \"state!\", kind FROM providers WHERE id = $1 FOR UPDATE",
            provider_id
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(ConfigurationError::NotFound)?;
        if provider.etag != expected_etag {
            return Err(ConfigurationError::PreconditionFailed);
        }
        if provider.state == "disabled" {
            return Err(ConfigurationError::InUse);
        }
        let provider_kind: String = provider.kind;
        for model in models {
            for capability in &model.capabilities {
                validate_provider_capability(&provider_kind, capability)?;
            }
        }
        for model in models {
            let model_id: Uuid = sqlx::query_scalar!(
                "INSERT INTO provider_models \
                 (id, provider_id, upstream_model, display_name, enabled, discovered_at) \
                 VALUES ($1, $2, $3, $4, $5, now()) \
                 ON CONFLICT (provider_id, upstream_model) DO UPDATE SET \
                   display_name = EXCLUDED.display_name, enabled = EXCLUDED.enabled, \
                   discovered_at = EXCLUDED.discovered_at RETURNING id",
                Uuid::now_v7(),
                provider_id,
                model.upstream_model.trim(),
                model.display_name.trim(),
                model.enabled
            )
            .fetch_one(&mut *transaction)
            .await?;
            sqlx::query!(
                "DELETE FROM model_capabilities WHERE provider_model_id = $1",
                model_id
            )
            .execute(&mut *transaction)
            .await?;
            for capability in &model.capabilities {
                validate_capability(capability)?;
                sqlx::query!(
                    "INSERT INTO model_capabilities \
                     (provider_model_id, operation, surface, mode, source, certified_at) \
                     VALUES ($1, $2, $3, $4, $5, CASE WHEN $5 = 'certified' THEN now() ELSE NULL END)",
                model_id, capability.operation.as_str(), capability.surface.as_str(), capability.mode.as_str(), capability.source.as_str())
                .execute(&mut *transaction)
                .await?;
            }
        }
        let etag = Uuid::now_v7();
        sqlx::query!(
            "UPDATE providers SET etag = $1, state = 'draft'::provider_state, updated_at = now(), \
                    last_probe_at = NULL, last_probe_status = NULL, last_probe_detail = NULL \
             WHERE id = $2",
            etag,
            provider_id
        )
        .execute(&mut *transaction)
        .await?;
        audit_in_transaction(
            &mut transaction,
            actor,
            "provider.discover",
            "provider",
            provider_id,
            "success",
        )
        .await?;
        transaction.commit().await?;
        Ok(etag)
    }

    pub async fn set_provider_model_enabled(
        &self,
        provider_id: Uuid,
        model_id: Uuid,
        enabled: bool,
        capabilities: &[CapabilityRecord],
        expected_etag: Uuid,
        actor: Uuid,
    ) -> Result<Uuid, ConfigurationError> {
        if enabled && capabilities.is_empty() {
            return Err(ConfigurationError::Invalid(
                "enabled models require at least one reviewed capability".to_owned(),
            ));
        }
        if capabilities.len() > 16 {
            return Err(ConfigurationError::Invalid(
                "a model can declare at most 16 capability tuples".to_owned(),
            ));
        }
        let mut unique = BTreeSet::new();
        for capability in capabilities {
            validate_capability(capability)?;
            let tuple = (
                capability.operation.as_str(),
                capability.surface.as_str(),
                capability.mode.as_str(),
            );
            if !unique.insert(tuple) {
                return Err(ConfigurationError::Invalid(
                    "model capabilities must be unique".to_owned(),
                ));
            }
        }
        let mut transaction = self.pool().begin().await?;
        let provider = sqlx::query!(
            "SELECT etag, state::text AS \"state!\", kind FROM providers WHERE id = $1 FOR UPDATE",
            provider_id
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(ConfigurationError::NotFound)?;
        if provider.etag != expected_etag {
            return Err(ConfigurationError::PreconditionFailed);
        }
        if provider.state == "disabled" {
            return Err(ConfigurationError::InUse);
        }
        let provider_kind: String = provider.kind;
        for capability in capabilities {
            validate_provider_capability(&provider_kind, capability)?;
        }
        let result = sqlx::query!(
            "UPDATE provider_models SET enabled = $1 WHERE id = $2 AND provider_id = $3",
            enabled,
            model_id,
            provider_id
        )
        .execute(&mut *transaction)
        .await?;
        if result.rows_affected() != 1 {
            return Err(ConfigurationError::NotFound);
        }
        sqlx::query!(
            "DELETE FROM model_capabilities WHERE provider_model_id = $1",
            model_id
        )
        .execute(&mut *transaction)
        .await?;
        for capability in capabilities {
            sqlx::query!(
                "INSERT INTO model_capabilities \
                 (provider_model_id, operation, surface, mode, source, certified_at) \
                 VALUES ($1, $2, $3, $4, $5, CASE WHEN $5 = 'certified' THEN now() ELSE NULL END)",
                model_id,
                capability.operation.as_str(),
                capability.surface.as_str(),
                capability.mode.as_str(),
                capability.source.as_str()
            )
            .execute(&mut *transaction)
            .await?;
        }
        let etag = Uuid::now_v7();
        sqlx::query!(
            "UPDATE providers SET etag = $1, state = 'draft'::provider_state, updated_at = now(), \
                    last_probe_at = NULL, last_probe_status = NULL, last_probe_detail = NULL \
             WHERE id = $2",
            etag,
            provider_id
        )
        .execute(&mut *transaction)
        .await?;
        audit_in_transaction(
            &mut transaction,
            actor,
            "provider.model.update",
            "provider_model",
            model_id,
            "success",
        )
        .await?;
        transaction.commit().await?;
        Ok(etag)
    }

    /// Applies evidence produced by a server-owned connector certifier. The
    /// submitted tuples must still exactly match the reviewed model
    /// capabilities under the supplied provider ETag. Every attempted tuple
    /// is first downgraded, and only successful checks are promoted. Native
    /// connector certification additionally requires fresh credentialed probe
    /// evidence for this exact draft; compatible endpoints execute a bounded
    /// per-tuple live probe in the HTTP layer.
    pub async fn apply_compatible_capability_certification(
        &self,
        provider_id: Uuid,
        model_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
        outcomes: &[CapabilityCertificationOutcome],
    ) -> Result<CapabilityCertificationApplied, ConfigurationError> {
        if outcomes.is_empty() || outcomes.len() > 16 {
            return Err(ConfigurationError::Invalid(
                "certification requires 1-16 reviewed capability tuples".to_owned(),
            ));
        }
        let mut submitted = BTreeSet::new();
        for outcome in outcomes {
            validate_capability(&CapabilityRecord {
                operation: outcome.operation,
                surface: outcome.surface,
                mode: outcome.mode,
                source: CapabilitySource::Declared,
                certified_at: None,
            })?;
            if !submitted.insert((outcome.operation, outcome.surface, outcome.mode)) {
                return Err(ConfigurationError::Invalid(
                    "certification capability tuples must be unique".to_owned(),
                ));
            }
        }

        let mut transaction = self.pool().begin().await?;
        let provider = sqlx::query!(
            "SELECT etag, state::text AS \"state!\", kind, updated_at, last_probe_at, \
                    last_probe_status \
             FROM providers WHERE id = $1 FOR UPDATE",
            provider_id
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(ConfigurationError::NotFound)?;
        if provider.etag != expected_etag {
            return Err(ConfigurationError::PreconditionFailed);
        }
        if provider.state != "draft" {
            return Err(ConfigurationError::InUse);
        }
        let provider_kind = provider.kind;
        if provider_kind != "openai_compatible" {
            let last_probe_at: Option<DateTime<Utc>> = provider.last_probe_at;
            let updated_at: DateTime<Utc> = provider.updated_at;
            let has_fresh_probe = provider.last_probe_status.as_deref() == Some("succeeded")
                && last_probe_at.is_some_and(|probed_at| probed_at >= updated_at);
            if !has_fresh_probe {
                return Err(ConfigurationError::Invalid(
                    "native capability certification requires a successful credentialed probe of the current provider draft"
                        .to_owned(),
                ));
            }
        }
        let discovered_at_row: Option<Option<DateTime<Utc>>> = sqlx::query_scalar!(
            "SELECT discovered_at FROM provider_models WHERE id = $1 AND provider_id = $2",
            model_id,
            provider_id
        )
        .fetch_optional(&mut *transaction)
        .await?;
        let Some(model_discovered_at) = discovered_at_row else {
            return Err(ConfigurationError::NotFound);
        };
        if provider_kind != "openai_compatible" && model_discovered_at.is_none() {
            return Err(ConfigurationError::Invalid(
                "native capability certification requires a discovered provider model".to_owned(),
            ));
        }
        let current = sqlx::query!(
            "SELECT operation, surface, mode FROM model_capabilities \
             WHERE provider_model_id = $1 FOR UPDATE",
            model_id
        )
        .fetch_all(&mut *transaction)
        .await?
        .into_iter()
        .map(|row| -> Result<_, PersistenceError> {
            Ok((
                row.operation
                    .parse()
                    .map_err(|_| PersistenceError::InvalidStoredValue("capability operation"))?,
                row.surface
                    .parse()
                    .map_err(|_| PersistenceError::InvalidStoredValue("capability surface"))?,
                row.mode.parse().map_err(|_| {
                    PersistenceError::InvalidStoredValue("capability transport mode")
                })?,
            ))
        })
        .collect::<Result<BTreeSet<_>, _>>()?;
        if current != submitted {
            return Err(ConfigurationError::PreconditionFailed);
        }

        let certified_at = Utc::now();
        sqlx::query!(
            "UPDATE model_capabilities SET source = 'declared', certified_at = NULL \
             WHERE provider_model_id = $1",
            model_id
        )
        .execute(&mut *transaction)
        .await?;
        let mut certified_count = 0_usize;
        for outcome in outcomes.iter().filter(|outcome| outcome.succeeded) {
            let updated = sqlx::query!(
                "UPDATE model_capabilities SET source = 'certified', certified_at = $1 \
                 WHERE provider_model_id = $2 AND operation = $3 AND surface = $4 AND mode = $5",
                certified_at,
                model_id,
                outcome.operation.as_str(),
                outcome.surface.as_str(),
                outcome.mode.as_str()
            )
            .execute(&mut *transaction)
            .await?;
            if updated.rows_affected() != 1 {
                return Err(ConfigurationError::PreconditionFailed);
            }
            certified_count += 1;
        }
        let etag = Uuid::now_v7();
        // Certification mutates reviewed evidence and therefore advances the
        // ETag, but it does not change transport configuration. Keeping
        // `updated_at` stable preserves the exact-config probe evidence that
        // was required above.
        sqlx::query!(
            "UPDATE providers SET etag = $1 WHERE id = $2 AND etag = $3",
            etag,
            provider_id,
            expected_etag
        )
        .execute(&mut *transaction)
        .await?;
        let audit_outcome = if certified_count == outcomes.len() {
            "success"
        } else if certified_count == 0 {
            "failure"
        } else {
            "partial"
        };
        audit_in_transaction(
            &mut transaction,
            actor,
            "provider.model.certify",
            "provider_model",
            model_id,
            audit_outcome,
        )
        .await?;
        transaction.commit().await?;
        Ok(CapabilityCertificationApplied {
            etag,
            certified_at,
            certified_count,
            attempted_count: outcomes.len(),
        })
    }
}

#[derive(Debug, sqlx::FromRow)]
struct ProviderModelRow {
    id: Uuid,
    upstream_model: String,
    display_name: String,
    enabled: bool,
    discovered_at: Option<DateTime<Utc>>,
}

#[derive(Debug, sqlx::FromRow)]
struct ModelCapabilityRow {
    provider_model_id: Uuid,
    operation: String,
    surface: String,
    mode: String,
    source: String,
    certified_at: Option<DateTime<Utc>>,
}

impl ModelCapabilityRow {
    fn split(self) -> (Uuid, CapabilityRow) {
        (
            self.provider_model_id,
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

#[derive(Debug, sqlx::FromRow)]
struct ProviderInventoryRow {
    id: Uuid,
    upstream_model: String,
    display_name: String,
    enabled: bool,
    discovered_at: Option<DateTime<Utc>>,
    provider_id: Uuid,
    provider_name: String,
    provider_kind: String,
}

impl ProviderInventoryRow {
    fn model(self) -> ProviderModelRow {
        ProviderModelRow {
            id: self.id,
            upstream_model: self.upstream_model,
            display_name: self.display_name,
            enabled: self.enabled,
            discovered_at: self.discovered_at,
        }
    }
}

async fn ensure_provider_exists(
    store: &PgStore,
    provider_id: Uuid,
) -> Result<(), ConfigurationError> {
    let exists: bool = sqlx::query_scalar!(
        "SELECT EXISTS (SELECT 1 FROM providers WHERE id = $1) AS \"value!\"",
        provider_id
    )
    .fetch_one(store.pool())
    .await?;
    exists.then_some(()).ok_or(ConfigurationError::NotFound)
}
