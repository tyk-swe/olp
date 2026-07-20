use super::*;

pub(super) fn capability_from_row(row: &PgRow) -> Result<CapabilityRecord, CatalogError> {
    Ok(CapabilityRecord {
        operation: row
            .get::<String, _>("operation")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("capability operation"))?,
        surface: row
            .get::<String, _>("surface")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("capability surface"))?,
        mode: row
            .get::<String, _>("mode")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("capability transport mode"))?,
        source: row
            .get::<String, _>("source")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("capability source"))?,
        certified_at: row.get("certified_at"),
    })
}

pub(super) fn catalog_count(row: &PgRow, column: &str) -> Result<u64, CatalogError> {
    u64::try_from(row.get::<i64, _>(column)).map_err(|_| {
        CatalogError::Invalid(format!(
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
) -> Result<(), CatalogError> {
    sqlx::query(
        "INSERT INTO audit_events (id, actor_user_id, action, resource_type, resource_id, outcome) \
         VALUES ($1, $2, $3, $4, $5, $6)",
    )
    .bind(Uuid::now_v7())
    .bind(actor)
    .bind(action)
    .bind(resource_type)
    .bind(resource_id.to_string())
    .bind(outcome)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}
