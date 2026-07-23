use super::*;

#[derive(Debug, sqlx::FromRow)]
pub(super) struct CapabilityRow {
    pub(super) operation: String,
    pub(super) surface: String,
    pub(super) mode: String,
    pub(super) source: String,
    pub(super) certified_at: Option<DateTime<Utc>>,
}

pub(super) fn capability_from_row(
    row: CapabilityRow,
) -> Result<CapabilityRecord, ConfigurationError> {
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

pub(super) fn checked_configuration_count(
    value: i64,
    column: &str,
) -> Result<u64, ConfigurationError> {
    u64::try_from(value).map_err(|_| {
        ConfigurationError::Invalid(format!(
            "stored provider {column} is outside the supported range"
        ))
    })
}

pub(super) async fn audit_in_transaction(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    action: &str,
    resource_type: &str,
    resource_id: Uuid,
    outcome: &str,
) -> Result<(), ConfigurationError> {
    sqlx::query!(
        "INSERT INTO audit_events (id, actor_user_id, action, resource_type, resource_id, outcome) \
         VALUES ($1, $2, $3, $4, $5, $6)",
        Uuid::now_v7(),
        actor,
        action,
        resource_type,
        resource_id.to_string(),
        outcome
    )
    .execute(&mut **transaction)
    .await?;
    Ok(())
}
