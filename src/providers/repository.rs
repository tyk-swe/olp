use crate::access::audit_events::AuditEvent;
use crate::access::audit_events::record_audit_event;
use crate::access::audit_events::record_success;
use crate::database::error::Error as PersistenceError;
use crate::database::idempotency::claim_idempotency;
use crate::database::idempotency::complete_idempotency;
use crate::database::page::ConfigurationPage;
use crate::database::query::split_page;
use crate::providers::error::Error;
use crate::providers::queries::checked_configuration_count;
use crate::providers::queries::lock_provider;
use crate::providers::record_validation::checked_limit;
use crate::providers::record_validation::transport_changed;
use crate::providers::record_validation::validate_provider_update;
use crate::providers::records::ProviderMutationResult;
use crate::providers::records::ProviderRecord;
use crate::providers::records::UpdateProvider;
use crate::runtime::publication::compiler::compile_and_publish_runtime_in_transaction;
use crate::runtime::publication::compiler::prepare_runtime_mutation;
use chrono::DateTime;
use chrono::Utc;
use std::collections::BTreeMap;
use uuid::Uuid;

/// Lists providers in id order; an empty `search` matches every provider.
pub async fn list_providers(
    pool: &sqlx::PgPool,
    cursor: Option<Uuid>,
    limit: i64,
    search: &str,
) -> Result<ConfigurationPage<ProviderRecord>, Error> {
    let limit = checked_limit(limit)?;
    let search = search.to_lowercase();
    let batch_size = if search.is_empty() {
        limit + 1
    } else {
        i64::from(crate::database::reads::MAX_PAGE_SIZE)
    };
    let mut after = cursor;
    let mut rows = Vec::new();
    loop {
        let candidates = sqlx::query_as::<_, ListProvidersRow>(
            "SELECT id, name, kind, endpoint, options->>'vendor_id' AS vendor_id
             FROM providers WHERE ($1::uuid IS NULL OR id > $1) ORDER BY id LIMIT $2",
        )
        .bind(after)
        .bind(batch_size)
        .fetch_all(pool)
        .await?;
        let exhausted = candidates.len() < batch_size as usize;
        after = candidates.last().map(|row| row.id);
        for row in candidates {
            if search.is_empty() || row.matches_search(&search)? {
                rows.push(row);
                if rows.len() > limit as usize {
                    break;
                }
            }
        }
        if rows.len() > limit as usize || exhausted {
            break;
        }
    }
    let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.id);
    let ids: Vec<Uuid> = rows.into_iter().map(|row| row.id).collect();
    let items =
        crate::providers::repository::get_providers(&mut *pool.acquire().await?, &ids).await?;
    Ok(ConfigurationPage { items, next_cursor })
}

/// Reads providers in the order of `ids`; the one projection every
/// provider read shares lives here. Any missing id is `NotFound`.
pub async fn get_providers(
    pool: &mut sqlx::PgConnection,
    ids: &[Uuid],
) -> Result<Vec<ProviderRecord>, Error> {
    if ids.is_empty() {
        return Ok(Vec::new());
    }
    let rows = sqlx::query_as::<_, ProviderRow>(
        "SELECT p.id AS \"id\", p.name AS \"name\", p.kind AS \"kind\", \
                    p.state::text AS \"state\", p.endpoint, p.cloud_region, \
                    p.cloud_project, p.deployment, p.api_version, p.auth_mode AS \"auth_mode\", p.options, \
                    p.connector_ready AS \"connector_ready\", \
                    p.etag AS \"etag\", ar.revision AS \"active_revision\", \
                    (p.state = 'draft'::provider_state AND p.active_revision_id IS NOT NULL) \
                      AS \"pending_activation\", \
                    p.active_credential_version_id AS draft_credential_id, \
                    draft_cv.version AS \"draft_credential_version\", \
                    ar.credential_version_id AS \"runtime_credential_id\", \
                    runtime_cv.version AS \"runtime_credential_version\", \
                    p.last_probe_at, p.last_probe_status, p.last_probe_detail, \
                    p.created_at AS \"created_at\", p.updated_at AS \"updated_at\", \
                    stats.model_count AS \"model_count\", \
                    stats.enabled_model_count AS \"enabled_model_count\", \
                    stats.capability_count AS \"capability_count\", \
                    stats.certified_capability_count AS \"certified_capability_count\", \
                    probe.upstream_model AS \"probe_model\", \
                    creator.email AS \"created_by_email\" \
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
    )
    .bind(ids)
    .fetch_all(pool)
    .await?;
    let mut by_id: BTreeMap<Uuid, ProviderRow> =
        rows.into_iter().map(|row| (row.id, row)).collect();
    ids.iter()
        .map(|id| by_id.remove(id).ok_or(Error::NotFound))
        .map(|row| row.and_then(provider_from_row))
        .collect()
}

pub async fn get_provider(pool: &sqlx::PgPool, provider_id: Uuid) -> Result<ProviderRecord, Error> {
    crate::providers::repository::get_providers(&mut *pool.acquire().await?, &[provider_id])
        .await?
        .into_iter()
        .next()
        .ok_or(Error::NotFound)
}

pub async fn update_provider(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    provider_id: Uuid,
    expected_etag: Uuid,
    update: &UpdateProvider,
    actor: Uuid,
) -> Result<Uuid, Error> {
    validate_provider_update(update)?;
    update
        .configuration
        .options
        .validate(update.configuration.kind)
        .map_err(Error::Invalid)?;
    let etag = Uuid::now_v7();
    let mut transaction = pool.begin().await?;
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
        sqlx::query(
            "UPDATE providers SET name = $1, options = $2, state = 'draft'::provider_state, \
                 etag = $3 WHERE id = $4",
        )
        .bind(update.name.trim())
        .bind(sqlx::types::Json(&update.configuration.options))
        .bind(etag)
        .bind(provider_id)
        .execute(&mut *transaction)
        .await?;
    }
    record_success(
        &mut *transaction,
        provenance,
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
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    provider_id: Uuid,
    expected_etag: Uuid,
    actor: Uuid,
    idempotency_key: &str,
) -> Result<ProviderMutationResult, Error> {
    let mut transaction = pool
        .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
        .await?;
    prepare_runtime_mutation(&mut transaction).await?;
    if !claim_idempotency(&mut transaction, actor, "provider.disable", idempotency_key).await? {
        return Err(Error::IdempotencyConflict);
    }
    let current = lock_provider(&mut transaction, provider_id)
        .await?
        .ok_or(Error::NotFound)?;
    let has_live_media_jobs: bool = sqlx::query_scalar::<_, bool>(
        "SELECT EXISTS (SELECT 1 FROM async_media_jobs
             WHERE provider_id = $1 AND lifecycle_state <> 'deleted') AS \"value\"",
    )
    .bind(provider_id)
    .fetch_one(&mut *transaction)
    .await?;
    if has_live_media_jobs {
        return Err(Error::InUse);
    }
    let referenced: bool = sqlx::query_scalar::<_, bool>(
        "SELECT EXISTS ( \
               SELECT 1 FROM routes r \
               JOIN LATERAL (SELECT id FROM route_revisions WHERE route_id = r.id \
                             ORDER BY revision DESC LIMIT 1) rr ON true \
               JOIN route_revision_targets rt ON rt.route_revision_id = rr.id \
               JOIN provider_models pm ON pm.id = rt.provider_model_id \
               WHERE pm.provider_id = $1 \
             ) AS \"value\"",
    )
    .bind(provider_id)
    .fetch_one(&mut *transaction)
    .await?;
    if referenced {
        return Err(Error::InUse);
    }
    let etag = Uuid::now_v7();
    let updated = sqlx::query(
        "UPDATE providers SET state = 'disabled'::provider_state, active_revision_id = NULL, \
                    etag = $1, updated_at = now() \
             WHERE id = $2 AND etag = $3 AND state <> 'disabled'::provider_state \
               AND active_revision_id IS NOT NULL",
    )
    .bind(etag)
    .bind(provider_id)
    .bind(expected_etag)
    .execute(&mut *transaction)
    .await?;
    if updated.rows_affected() != 1 {
        return Err(if current.etag != expected_etag {
            Error::PreconditionFailed
        } else {
            Error::InUse
        });
    }
    record_success(
        &mut *transaction,
        provenance,
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
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    provider_id: Uuid,
    expected_etag: Uuid,
    actor: Uuid,
    idempotency_key: &str,
) -> Result<Uuid, Error> {
    let mut transaction = pool.begin().await?;
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
    let updated = sqlx::query(
        "UPDATE providers SET state = 'draft'::provider_state, etag = $1, updated_at = now(), \
                    last_probe_at = NULL, last_probe_status = NULL, last_probe_detail = NULL \
             WHERE id = $2 AND etag = $3 AND state = 'disabled'::provider_state",
    )
    .bind(etag)
    .bind(provider_id)
    .bind(expected_etag)
    .execute(&mut *transaction)
    .await?;
    if updated.rows_affected() != 1 {
        let row = sqlx::query_as::<_, RestoreProviderAsDraftRow>(
            "SELECT etag FROM providers WHERE id = $1",
        )
        .bind(provider_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(Error::NotFound)?;
        return Err(if row.etag != expected_etag {
            Error::PreconditionFailed
        } else {
            Error::InUse
        });
    }
    sqlx::query(
        "UPDATE model_capabilities SET source = 'declared', certified_at = NULL \
             WHERE provider_model_id IN (SELECT id FROM provider_models WHERE provider_id = $1)",
    )
    .bind(provider_id)
    .execute(&mut *transaction)
    .await?;
    record_success(
        &mut *transaction,
        provenance,
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
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
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
    let mut transaction = pool.begin().await?;
    let result = sqlx::query(
        "UPDATE providers SET last_probe_at = $1, last_probe_status = $2, \
                    last_probe_detail = $3 WHERE id = $4 AND etag = $5",
    )
    .bind(at)
    .bind(if succeeded { "succeeded" } else { "failed" })
    .bind(detail)
    .bind(provider_id)
    .bind(expected_etag)
    .execute(&mut *transaction)
    .await?;
    if result.rows_affected() != 1 {
        let current_etag: Option<Uuid> =
            sqlx::query_scalar::<_, uuid::Uuid>("SELECT etag FROM providers WHERE id = $1")
                .bind(provider_id)
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
            provenance,
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

#[derive(Debug, sqlx::FromRow)]
struct ProviderRow {
    id: Uuid,
    name: String,
    #[sqlx(flatten)]
    configuration: crate::providers::configuration::ProviderConfiguration,
    state: String,

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
        configuration: row.configuration,
        state: row
            .state
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("provider state"))?,

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
    sqlx::query(
        "UPDATE providers SET name = $1, endpoint = $2, cloud_region = $3, cloud_project = $4, \
                deployment = $5, api_version = $6, auth_mode = $7, options = $8, \
                active_credential_version_id = CASE \
                  WHEN $7 IN ('adc', 'default_chain', 'none') THEN NULL \
                  ELSE active_credential_version_id END, \
                state = 'draft'::provider_state, etag = $9, updated_at = now(), \
                last_probe_at = NULL, last_probe_status = NULL, last_probe_detail = NULL \
         WHERE id = $10",
    )
    .bind(update.name.trim())
    .bind(update.configuration.endpoint.as_deref().map(str::trim))
    .bind(update.configuration.cloud_region.as_deref().map(str::trim))
    .bind(update.configuration.cloud_project.as_deref().map(str::trim))
    .bind(update.configuration.deployment.as_deref().map(str::trim))
    .bind(update.configuration.api_version.as_deref().map(str::trim))
    .bind(update.configuration.auth_mode.as_str())
    .bind(sqlx::types::Json(&update.configuration.options))
    .bind(etag)
    .bind(provider_id)
    .execute(&mut **transaction)
    .await?;
    sqlx::query(
        "UPDATE model_capabilities SET source = 'declared', certified_at = NULL \
         WHERE provider_model_id IN \
           (SELECT id FROM provider_models WHERE provider_id = $1)",
    )
    .bind(provider_id)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

#[derive(sqlx::FromRow)]
struct ListProvidersRow {
    id: uuid::Uuid,
    name: String,
    kind: String,
    endpoint: Option<String>,
    vendor_id: Option<String>,
}

impl ListProvidersRow {
    fn matches_search(&self, search: &str) -> Result<bool, Error> {
        let kind = self
            .kind
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("provider kind"))?;
        let vendor = crate::providers::catalog::effective_vendor(
            self.vendor_id.as_deref(),
            kind,
            self.endpoint.as_deref(),
        );
        Ok(
            format!("{} {} {}", self.name, self.kind, vendor.unwrap_or_default())
                .to_lowercase()
                .contains(search),
        )
    }
}

#[derive(sqlx::FromRow)]
struct RestoreProviderAsDraftRow {
    etag: uuid::Uuid,
}

#[cfg(test)]
mod search_tests {
    use super::*;

    #[tokio::test]
    #[ignore = "requires PostgreSQL via make integration"]
    async fn inferred_vendor_search_filters_before_pagination() {
        let db = crate::test_support::TestDb::create_migrated("provider_vendor_search").await;
        let pool = db.pool(2).await;
        let actor = Uuid::now_v7();
        sqlx::query("INSERT INTO users(id,email,display_name,role) VALUES($1,'vendor-search@test.example','Owner','owner')")
            .bind(actor).execute(&pool).await.unwrap();
        let fixtures = [
            ("gemini", Some("https://custom.example.test"), None),
            ("openai", None, None),
            ("gemini", None, None),
            ("vertex_ai", None, None),
            (
                "openai_compatible",
                Some("https://openrouter.ai/api/v1/"),
                None,
            ),
            (
                "gemini",
                Some("https://custom.example.test"),
                Some("google"),
            ),
            (
                "openai_compatible",
                Some("https://openrouter.ai/api/v1/"),
                Some("deepseek"),
            ),
        ];
        let mut ids = Vec::new();
        for (index, (kind, endpoint, vendor)) in fixtures.into_iter().enumerate() {
            let id = Uuid::from_u128(index as u128 + 1);
            ids.push(id);
            let options = vendor.map_or_else(
                || serde_json::json!({}),
                |vendor| serde_json::json!({"vendor_id":vendor}),
            );
            sqlx::query("INSERT INTO providers(id,name,kind,endpoint,auth_mode,etag,created_by,options) VALUES($1,$2,$3,$4,$5,$6,$7,$8)")
                .bind(id).bind(format!("connection-{index}")).bind(kind).bind(endpoint)
                .bind(if kind == "vertex_ai" { "adc" } else { "api_key" })
                .bind(Uuid::now_v7()).bind(actor).bind(options).execute(&pool).await.unwrap();
        }
        // Unmatched rows can span several scan batches before a search result.
        sqlx::query("INSERT INTO providers(id,name,kind,auth_mode,etag,created_by) SELECT uuidv7(),'unmatched-'||n,'openai','api_key',uuidv7(),$1 FROM generate_series(1,$2) AS n")
            .bind(actor).bind(i32::from(crate::database::reads::MAX_PAGE_SIZE) + 1)
            .execute(&pool).await.unwrap();
        let last = Uuid::from_u128(u128::MAX);
        sqlx::query("INSERT INTO providers(id,name,kind,auth_mode,etag,created_by) VALUES($1,'last-connection','gemini','api_key',$2,$3)")
            .bind(last).bind(Uuid::now_v7()).bind(actor).execute(&pool).await.unwrap();

        let mut cursor = None;
        for (index, expected) in [ids[2], ids[3], ids[5], last].into_iter().enumerate() {
            let page = list_providers(&pool, cursor, 1, "GOOGLE").await.unwrap();
            assert_eq!(page.items.len(), 1);
            assert_eq!(page.items[0].id, expected);
            assert_eq!(page.next_cursor.is_some(), index < 3);
            cursor = page.next_cursor;
        }
        for (search, expected) in [("openrouter", ids[4]), ("deepseek", ids[6])] {
            let page = list_providers(&pool, None, 1, search).await.unwrap();
            assert_eq!(page.items.len(), 1);
            assert_eq!(page.items[0].id, expected);
            assert!(page.next_cursor.is_none());
        }
        let page = list_providers(&pool, None, 1, "absent-vendor")
            .await
            .unwrap();
        assert!(page.items.is_empty());
        assert!(page.next_cursor.is_none());
    }
}
