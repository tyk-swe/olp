use crate::database::error::Error as PersistenceError;
use crate::providers::error::Error;
use crate::providers::records::CapabilityRecord;
use chrono::DateTime;
use chrono::Utc;
use uuid::Uuid;

#[derive(Debug, sqlx::FromRow)]
pub(crate) struct CapabilityRow {
    pub(crate) operation: String,
    pub(crate) surface: String,
    pub(crate) mode: String,
    pub(crate) source: String,
    pub(crate) certified_at: Option<DateTime<Utc>>,
}

pub(crate) fn capability_from_row(row: CapabilityRow) -> Result<CapabilityRecord, Error> {
    Ok(CapabilityRecord {
        operation: row
            .operation
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("capability operation"))?,
        surface: row
            .surface
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("capability surface"))?,
        mode: row
            .mode
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("capability transport mode"))?,
        source: row
            .source
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("capability source"))?,
        certified_at: row.certified_at,
    })
}

pub(crate) fn checked_configuration_count(value: i64, column: &str) -> Result<u64, Error> {
    u64::try_from(value).map_err(|_| {
        Error::Invalid(format!(
            "stored provider {column} is outside the supported range"
        ))
    })
}

/// The provider row every mutation locks first. A superset of the columns the
/// callers compare, so each site takes the same `FOR UPDATE` read.
#[derive(sqlx::FromRow)]
pub(crate) struct LockedProvider {
    pub(crate) etag: Uuid,
    pub(crate) state: String,
    #[sqlx(flatten)]
    pub(crate) configuration: crate::providers::configuration::ProviderConfiguration,

    pub(crate) active_credential_version_id: Option<Uuid>,
    pub(crate) updated_at: DateTime<Utc>,
    pub(crate) last_probe_at: Option<DateTime<Utc>>,
    pub(crate) last_probe_status: Option<String>,
}

/// Locks a provider row for the rest of the transaction; `None` when it does
/// not exist, so the caller picks its own not-found error.
pub(crate) async fn lock_provider(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    provider_id: Uuid,
) -> Result<Option<LockedProvider>, sqlx::Error> {
    sqlx::query_as::<_, LockedProvider>(
        "SELECT etag, state::text AS \"state\", kind, endpoint, cloud_region, cloud_project, \
                deployment, api_version, auth_mode, options, active_credential_version_id, \
                updated_at, last_probe_at, last_probe_status \
         FROM providers WHERE id = $1 FOR UPDATE",
    )
    .bind(provider_id)
    .fetch_optional(&mut **transaction)
    .await
}
