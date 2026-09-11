use crate::access::audit_events::AuditEvent;
use crate::access::audit_events::record_audit_event;
use crate::access::audit_events::record_success;
use crate::database::error::Error as PersistenceError;
use crate::database::page::ConfigurationPage;
use crate::database::query::split_page;
use crate::protocols::canonical::identity::OperationKind;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::canonical::identity::TransportMode;
use crate::providers::error::Error;
use crate::providers::queries::CapabilityRow;
use crate::providers::queries::capability_from_row;
use crate::providers::queries::lock_provider;
use crate::providers::record_validation::MAX_MODEL_CAPABILITY_TUPLES;
use crate::providers::record_validation::checked_limit;
use crate::providers::record_validation::validate_model;
use crate::providers::record_validation::validate_provider_capability;
use crate::providers::records::CapabilityCertificationApplied;
use crate::providers::records::CapabilityCertificationOutcome;
use crate::providers::records::CapabilityRecord;
use crate::providers::records::DiscoveredModelInput;
use crate::providers::records::ProviderModelInventoryRecord;
use crate::providers::records::ProviderModelPage;
use crate::providers::records::ProviderModelRecord;
use crate::providers::runtime_model::ProviderKind;
use chrono::DateTime;
use chrono::Utc;
use std::collections::BTreeMap;
use std::collections::BTreeSet;
use uuid::Uuid;

pub async fn list_provider_models(
    pool: &sqlx::PgPool,
    provider_id: Uuid,
    cursor: Option<Uuid>,
    limit: i64,
) -> Result<ProviderModelPage, Error> {
    let limit = checked_limit(limit)?;
    let mut transaction = pool
        .begin_with("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY")
        .await?;
    let provider = crate::providers::repository::get_providers(&mut transaction, &[provider_id])
        .await?
        .pop()
        .ok_or(Error::NotFound)?;
    let rows = sqlx::query_as::<_, ProviderModelRow>(
        "SELECT id, upstream_model, display_name, enabled, discovered_at \
             FROM provider_models WHERE provider_id = $1 \
               AND ($2::uuid IS NULL OR id > $2) ORDER BY id LIMIT $3",
    )
    .bind(provider_id)
    .bind(cursor)
    .bind(limit + 1)
    .fetch_all(&mut *transaction)
    .await?;
    let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.id);
    let items =
        crate::providers::models::provider_models_from_rows(&mut *transaction, rows).await?;
    transaction.commit().await?;
    Ok(ProviderModelPage {
        provider,
        items,
        next_cursor,
    })
}

pub async fn list_provider_model_inventory(
    pool: &sqlx::PgPool,
    cursor: Option<Uuid>,
    limit: i64,
    enabled: Option<bool>,
) -> Result<ConfigurationPage<ProviderModelInventoryRecord>, Error> {
    list_provider_model_inventory_filtered(pool, cursor, limit, enabled, "", None).await
}

pub async fn list_provider_model_inventory_filtered(
    pool: &sqlx::PgPool,
    cursor: Option<Uuid>,
    limit: i64,
    enabled: Option<bool>,
    search: &str,
    surface: Option<&str>,
) -> Result<ConfigurationPage<ProviderModelInventoryRecord>, Error> {
    let limit = checked_limit(limit)?;
    let rows = sqlx::query_as::<_, ProviderInventoryRow>(
        "SELECT pm.id, pm.upstream_model, pm.display_name, pm.enabled, pm.discovered_at, \
                    EXISTS(SELECT 1 FROM provider_revision_models prm WHERE prm.provider_revision_id=p.active_revision_id AND prm.source_provider_model_id=pm.id AND prm.enabled AND p.state <> 'disabled') AS available, COALESCE(p.options->'models'->pm.upstream_model,'{}'::jsonb) AS metadata, p.id AS provider_id, p.name AS provider_name, p.kind AS provider_kind \
             FROM provider_models pm JOIN providers p ON p.id = pm.provider_id \
             WHERE ($1::uuid IS NULL OR pm.id > $1) \
               AND ($2::boolean IS NULL OR pm.enabled = $2) \
               AND ($4 = '' OR strpos(lower(p.name || ' ' || pm.upstream_model || ' ' || pm.display_name || ' ' || COALESCE(p.options->>'vendor_id','') || ' ' || COALESCE(p.options->'models'->pm.upstream_model->>'canonical_model','') || ' ' || COALESCE(p.options->'models'->pm.upstream_model->>'region','')), lower($4)) > 0) \
               AND ($5::text IS NULL OR EXISTS(SELECT 1 FROM model_capabilities mc WHERE mc.provider_model_id=pm.id AND mc.surface=$5)) \
             ORDER BY pm.id LIMIT $3",
    )
    .bind(cursor)
    .bind(enabled)
    .bind(limit + 1)
    .bind(search)
    .bind(surface)
    .fetch_all(pool)
    .await?;
    let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.id);
    let mut providers = rows
        .iter()
        .map(|row| {
            Ok((
                row.id,
                (
                    row.available,
                    row.metadata.0.clone(),
                    row.provider_id,
                    row.provider_name.clone(),
                    row.provider_kind
                        .parse()
                        .map_err(|_| PersistenceError::InvalidStoredValue("provider kind"))?,
                ),
            ))
        })
        .collect::<Result<BTreeMap<_, _>, Error>>()?;
    let model_rows = rows.into_iter().map(ProviderInventoryRow::model).collect();
    let items = crate::providers::models::provider_models_from_rows(pool, model_rows)
        .await?
        .into_iter()
        .map(|model| {
            let (available, metadata, provider_id, provider_name, provider_kind) = providers
                .remove(&model.id)
                .ok_or(PersistenceError::InvalidStoredValue(
                    "provider metadata for model",
                ))?;
            Ok(ProviderModelInventoryRecord {
                available,
                metadata,
                provider_id,
                provider_name,
                provider_kind,
                model,
            })
        })
        .collect::<Result<Vec<_>, Error>>()?;
    Ok(ConfigurationPage { items, next_cursor })
}

pub async fn get_provider_model(
    pool: &sqlx::PgPool,
    provider_id: Uuid,
    model_id: Uuid,
) -> Result<ProviderModelRecord, Error> {
    let rows = sqlx::query_as::<_, ProviderModelRow>(
        "SELECT id, upstream_model, display_name, enabled, discovered_at \
             FROM provider_models WHERE provider_id = $1 AND id = $2",
    )
    .bind(provider_id)
    .bind(model_id)
    .fetch_all(pool)
    .await?;
    crate::providers::models::provider_models_from_rows(pool, rows)
        .await?
        .into_iter()
        .next()
        .ok_or(Error::NotFound)
}

async fn provider_models_from_rows(
    executor: impl sqlx::PgExecutor<'_>,
    rows: Vec<ProviderModelRow>,
) -> Result<Vec<ProviderModelRecord>, Error> {
    let model_ids = rows.iter().map(|row| row.id).collect::<Vec<_>>();
    let capability_rows = if model_ids.is_empty() {
        Vec::new()
    } else {
        sqlx::query_as::<_, ModelCapabilityRow>(
            "SELECT provider_model_id, operation, surface, mode, source, certified_at \
                 FROM model_capabilities WHERE provider_model_id = ANY($1::uuid[]) \
                 ORDER BY provider_model_id, operation, surface, mode",
        )
        .bind(&model_ids)
        .fetch_all(executor)
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
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    provider_id: Uuid,
    expected_etag: Uuid,
    models: &[DiscoveredModelInput],
    actor: Uuid,
) -> Result<Uuid, Error> {
    if models.is_empty() {
        return Err(Error::Invalid("discovery returned no models".to_owned()));
    }
    let mut names = BTreeSet::new();
    for model in models {
        validate_model(model)?;
        if !names.insert(model.upstream_model.trim()) {
            return Err(Error::Invalid("model names must be unique".to_owned()));
        }
    }
    let mut transaction = pool.begin().await?;
    let provider = lock_provider(&mut transaction, provider_id)
        .await?
        .ok_or(Error::NotFound)?;
    if provider.etag != expected_etag {
        return Err(Error::PreconditionFailed);
    }
    if provider.state == "disabled" {
        return Err(Error::InUse);
    }
    let provider_kind = provider.configuration.kind;
    for model in models {
        for capability in &model.capabilities {
            validate_provider_capability(provider_kind.as_str(), capability)?;
        }
    }
    store_discovered_models(&mut transaction, provider_id, models).await?;
    let etag = Uuid::now_v7();
    // The `FOR UPDATE` read above already refused a disabled provider, and
    // it holds the row for the rest of the transaction. The guard repeats
    // that refusal in the write itself so no future edit can turn discovery
    // into a path that resurrects a disabled provider as a draft, skipping
    // the `restore_as_draft` ceremony.
    let restored = sqlx::query(
        "UPDATE providers SET etag = $1, state = 'draft'::provider_state \
             WHERE id = $2 AND state <> 'disabled'::provider_state",
    )
    .bind(etag)
    .bind(provider_id)
    .execute(&mut *transaction)
    .await?;
    if restored.rows_affected() != 1 {
        return Err(Error::InUse);
    }
    record_success(
        &mut *transaction,
        provenance,
        actor,
        "provider.discover",
        "provider",
        provider_id,
    )
    .await?;
    transaction.commit().await?;
    Ok(etag)
}

#[allow(clippy::too_many_arguments)]
pub async fn set_provider_model_enabled(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    provider_id: Uuid,
    model_id: Uuid,
    enabled: bool,
    capabilities: &[CapabilityRecord],
    expected_etag: Uuid,
    actor: Uuid,
) -> Result<Uuid, Error> {
    if enabled && capabilities.is_empty() {
        return Err(Error::Invalid(
            "enabled models require at least one reviewed capability".to_owned(),
        ));
    }
    if capabilities.len() > MAX_MODEL_CAPABILITY_TUPLES {
        return Err(Error::Invalid(format!(
            "a model can declare at most {MAX_MODEL_CAPABILITY_TUPLES} capability tuples"
        )));
    }
    let mut unique = BTreeSet::new();
    for capability in capabilities {
        let tuple = (
            capability.operation.as_str(),
            capability.surface.as_str(),
            capability.mode.as_str(),
        );
        if !unique.insert(tuple) {
            return Err(Error::Invalid(
                "model capabilities must be unique".to_owned(),
            ));
        }
    }
    let mut transaction = pool.begin().await?;
    let provider = lock_provider(&mut transaction, provider_id)
        .await?
        .ok_or(Error::NotFound)?;
    if provider.etag != expected_etag {
        return Err(Error::PreconditionFailed);
    }
    if provider.state == "disabled" {
        return Err(Error::InUse);
    }
    let provider_kind = provider.configuration.kind;
    for capability in capabilities {
        validate_provider_capability(provider_kind.as_str(), capability)?;
    }
    let result =
        sqlx::query("UPDATE provider_models SET enabled = $1 WHERE id = $2 AND provider_id = $3")
            .bind(enabled)
            .bind(model_id)
            .bind(provider_id)
            .execute(&mut *transaction)
            .await?;
    if result.rows_affected() != 1 {
        return Err(Error::NotFound);
    }
    replace_model_capabilities(&mut transaction, model_id, capabilities).await?;
    let etag = Uuid::now_v7();
    // Same guard as discovery: capability review must never be the door a
    // disabled provider walks back through into `draft`.
    let restored = sqlx::query(
        "UPDATE providers SET etag = $1, state = 'draft'::provider_state \
             WHERE id = $2 AND state <> 'disabled'::provider_state",
    )
    .bind(etag)
    .bind(provider_id)
    .execute(&mut *transaction)
    .await?;
    if restored.rows_affected() != 1 {
        return Err(Error::InUse);
    }
    record_success(
        &mut *transaction,
        provenance,
        actor,
        "provider.model.update",
        "provider_model",
        model_id,
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
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    provider_id: Uuid,
    model_id: Uuid,
    expected_etag: Uuid,
    actor: Uuid,
    outcomes: &[CapabilityCertificationOutcome],
) -> Result<CapabilityCertificationApplied, Error> {
    if outcomes.is_empty() || outcomes.len() > MAX_MODEL_CAPABILITY_TUPLES {
        return Err(Error::Invalid(format!(
            "certification requires 1-{MAX_MODEL_CAPABILITY_TUPLES} reviewed capability tuples"
        )));
    }
    let mut submitted = BTreeSet::new();
    for outcome in outcomes {
        if !submitted.insert((outcome.operation, outcome.surface, outcome.mode)) {
            return Err(Error::Invalid(
                "certification capability tuples must be unique".to_owned(),
            ));
        }
    }

    let mut transaction = pool.begin().await?;
    validate_certification_evidence(
        &mut transaction,
        provider_id,
        model_id,
        expected_etag,
        &submitted,
    )
    .await?;
    let certified_at = Utc::now();
    sqlx::query(
        "UPDATE model_capabilities SET source = 'declared', certified_at = NULL \
             WHERE provider_model_id = $1",
    )
    .bind(model_id)
    .execute(&mut *transaction)
    .await?;
    let mut certified_count = 0_usize;
    for outcome in outcomes.iter().filter(|outcome| outcome.succeeded) {
        let updated = sqlx::query(
            "UPDATE model_capabilities SET source = 'certified', certified_at = $1 \
                 WHERE provider_model_id = $2 AND operation = $3 AND surface = $4 AND mode = $5",
        )
        .bind(certified_at)
        .bind(model_id)
        .bind(outcome.operation.as_str())
        .bind(outcome.surface.as_str())
        .bind(outcome.mode.as_str())
        .execute(&mut *transaction)
        .await?;
        if updated.rows_affected() != 1 {
            return Err(Error::PreconditionFailed);
        }
        certified_count += 1;
    }
    let etag = Uuid::now_v7();
    // Certification mutates reviewed evidence and therefore advances the
    // ETag, but it does not change transport configuration. Keeping
    // `updated_at` stable preserves the exact-config probe evidence that
    // was required above.
    sqlx::query("UPDATE providers SET etag = $1 WHERE id = $2 AND etag = $3")
        .bind(etag)
        .bind(provider_id)
        .bind(expected_etag)
        .execute(&mut *transaction)
        .await?;
    let audit_outcome = if certified_count == outcomes.len() {
        "success"
    } else if certified_count == 0 {
        "failure"
    } else {
        "partial"
    };
    record_audit_event(
        &mut *transaction,
        AuditEvent {
            provenance,
            actor: Some(actor),
            action: "provider.model.certify",
            resource_type: "provider_model",
            resource_id: Some(&model_id.to_string()),
            outcome: audit_outcome,
            occurred_at: None,
        },
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
    available: bool,
    metadata: sqlx::types::Json<crate::providers::options::ModelMetadata>,
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

/// Upserts every discovered model in one statement and reconciles their
/// capability rows in two more, keeping evidence on tuples that persist.
async fn store_discovered_models(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    provider_id: Uuid,
    models: &[DiscoveredModelInput],
) -> Result<(), Error> {
    // Every model is upserted in one statement; the returned ids key the
    // capability rewrite that follows.
    let ids = models.iter().map(|_| Uuid::now_v7()).collect::<Vec<_>>();
    let upstream_models = models
        .iter()
        .map(|model| model.upstream_model.trim().to_owned())
        .collect::<Vec<_>>();
    let display_names = models
        .iter()
        .map(|model| model.display_name.trim().to_owned())
        .collect::<Vec<_>>();
    let enabled = models.iter().map(|model| model.enabled).collect::<Vec<_>>();
    let model_ids: BTreeMap<String, Uuid> = sqlx::query_as::<_, StoreDiscoveredModelsRow>(
        "INSERT INTO provider_models \
         (id, provider_id, upstream_model, display_name, enabled, discovered_at) \
         SELECT t.id, $1, t.upstream_model, t.display_name, t.enabled, now() \
         FROM UNNEST($2::uuid[], $3::text[], $4::text[], $5::bool[]) \
           AS t(id, upstream_model, display_name, enabled) \
         ON CONFLICT (provider_id, upstream_model) DO UPDATE SET \
           display_name = EXCLUDED.display_name, enabled = EXCLUDED.enabled, \
           discovered_at = EXCLUDED.discovered_at \
         RETURNING id, upstream_model",
    )
    .bind(provider_id)
    .bind(&ids)
    .bind(&upstream_models)
    .bind(&display_names)
    .bind(&enabled)
    .fetch_all(&mut **transaction)
    .await?
    .into_iter()
    .map(|row| (row.upstream_model, row.id))
    .collect();
    let stored_ids = model_ids.values().copied().collect::<Vec<_>>();
    let mut capability_model_ids = Vec::new();
    let mut operations = Vec::new();
    let mut surfaces = Vec::new();
    let mut modes = Vec::new();
    let mut sources = Vec::new();
    for model in models {
        let model_id = *model_ids
            .get(model.upstream_model.trim())
            .ok_or_else(|| Error::Invalid("discovered model was not stored".to_owned()))?;
        for capability in &model.capabilities {
            capability_model_ids.push(model_id);
            operations.push(capability.operation.as_str().to_owned());
            surfaces.push(capability.surface.as_str().to_owned());
            modes.push(capability.mode.as_str().to_owned());
            sources.push(capability.source.as_str().to_owned());
        }
    }
    sqlx::query(
        "DELETE FROM model_capabilities WHERE provider_model_id = ANY($1::uuid[]) \
           AND (provider_model_id, operation, surface, mode) NOT IN \
             (SELECT * FROM UNNEST($2::uuid[], $3::text[], $4::text[], $5::text[]))",
    )
    .bind(&stored_ids)
    .bind(&capability_model_ids)
    .bind(&operations)
    .bind(&surfaces)
    .bind(&modes)
    .execute(&mut **transaction)
    .await?;
    sqlx::query(
        "INSERT INTO model_capabilities \
         (provider_model_id, operation, surface, mode, source, certified_at) \
         SELECT t.provider_model_id, t.operation, t.surface, t.mode, t.source, \
                CASE WHEN t.source = 'certified' THEN now() ELSE NULL END \
         FROM UNNEST($1::uuid[], $2::text[], $3::text[], $4::text[], $5::text[]) \
           AS t(provider_model_id, operation, surface, mode, source) \
         ON CONFLICT DO NOTHING",
    )
    .bind(&capability_model_ids)
    .bind(&operations)
    .bind(&surfaces)
    .bind(&modes)
    .bind(&sources)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

/// Rewrites one model's reviewed tuple set while keeping the evidence on
/// every tuple that survives the review: removed tuples are deleted, new ones
/// are inserted, unchanged ones are left untouched.
async fn replace_model_capabilities(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    model_id: Uuid,
    capabilities: &[CapabilityRecord],
) -> Result<(), Error> {
    let operations = capabilities
        .iter()
        .map(|capability| capability.operation.as_str().to_owned())
        .collect::<Vec<_>>();
    let surfaces = capabilities
        .iter()
        .map(|capability| capability.surface.as_str().to_owned())
        .collect::<Vec<_>>();
    let modes = capabilities
        .iter()
        .map(|capability| capability.mode.as_str().to_owned())
        .collect::<Vec<_>>();
    let sources = capabilities
        .iter()
        .map(|capability| capability.source.as_str().to_owned())
        .collect::<Vec<_>>();
    sqlx::query(
        "DELETE FROM model_capabilities WHERE provider_model_id = $1 \
           AND (operation, surface, mode) NOT IN \
             (SELECT * FROM UNNEST($2::text[], $3::text[], $4::text[]))",
    )
    .bind(model_id)
    .bind(&operations)
    .bind(&surfaces)
    .bind(&modes)
    .execute(&mut **transaction)
    .await?;
    sqlx::query(
        "INSERT INTO model_capabilities \
         (provider_model_id, operation, surface, mode, source, certified_at) \
         SELECT $1, t.operation, t.surface, t.mode, t.source, \
                CASE WHEN t.source = 'certified' THEN now() ELSE NULL END \
         FROM UNNEST($2::text[], $3::text[], $4::text[], $5::text[]) \
           AS t(operation, surface, mode, source) \
         ON CONFLICT DO NOTHING",
    )
    .bind(model_id)
    .bind(&operations)
    .bind(&surfaces)
    .bind(&modes)
    .bind(&sources)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

async fn validate_certification_evidence(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    provider_id: Uuid,
    model_id: Uuid,
    expected_etag: Uuid,
    submitted: &BTreeSet<(OperationKind, Surface, TransportMode)>,
) -> Result<(), Error> {
    let provider = lock_provider(transaction, provider_id)
        .await?
        .ok_or(Error::NotFound)?;
    if provider.etag != expected_etag {
        return Err(Error::PreconditionFailed);
    }
    if provider.state != "draft" {
        return Err(Error::InUse);
    }
    let provider_kind = provider.configuration.kind;
    if provider_kind != ProviderKind::OpenAiCompatible {
        let last_probe_at: Option<DateTime<Utc>> = provider.last_probe_at;
        let updated_at: DateTime<Utc> = provider.updated_at;
        let has_fresh_probe = provider.last_probe_status.as_deref() == Some("succeeded")
            && last_probe_at.is_some_and(|probed_at| probed_at >= updated_at);
        if !has_fresh_probe {
            return Err(Error::Invalid(
                    "native capability certification requires a successful credentialed probe of the current provider draft"
                        .to_owned(),
                ));
        }
    }
    let discovered_at_row: Option<Option<DateTime<Utc>>> =
        sqlx::query_scalar::<_, Option<chrono::DateTime<chrono::Utc>>>(
            "SELECT discovered_at FROM provider_models WHERE id = $1 AND provider_id = $2",
        )
        .bind(model_id)
        .bind(provider_id)
        .fetch_optional(&mut **transaction)
        .await?;
    let Some(model_discovered_at) = discovered_at_row else {
        return Err(Error::NotFound);
    };
    if provider_kind != ProviderKind::OpenAiCompatible && model_discovered_at.is_none() {
        return Err(Error::Invalid(
            "native capability certification requires a discovered provider model".to_owned(),
        ));
    }
    let current = sqlx::query_as::<_, ValidateCertificationEvidenceRow>(
        "SELECT operation, surface, mode FROM model_capabilities \
             WHERE provider_model_id = $1 FOR UPDATE",
    )
    .bind(model_id)
    .fetch_all(&mut **transaction)
    .await?
    .into_iter()
    .map(|row| -> Result<_, Error> {
        Ok((
            row.operation
                .parse()
                .map_err(|_| PersistenceError::InvalidStoredValue("capability operation"))?,
            row.surface
                .parse()
                .map_err(|_| PersistenceError::InvalidStoredValue("capability surface"))?,
            row.mode
                .parse()
                .map_err(|_| PersistenceError::InvalidStoredValue("capability transport mode"))?,
        ))
    })
    .collect::<Result<BTreeSet<_>, _>>()?;
    if &current != submitted {
        return Err(Error::PreconditionFailed);
    }

    Ok(())
}

#[derive(sqlx::FromRow)]
struct StoreDiscoveredModelsRow {
    id: uuid::Uuid,
    upstream_model: String,
}

#[derive(sqlx::FromRow)]
struct ValidateCertificationEvidenceRow {
    operation: String,
    surface: String,
    mode: String,
}
