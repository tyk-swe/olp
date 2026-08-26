use super::*;

#[derive(Debug, sqlx::FromRow)]
pub(super) struct CapabilityRow {
    pub(super) operation: String,
    pub(super) surface: String,
    pub(super) mode: String,
    pub(super) source: String,
    pub(super) certified_at: Option<DateTime<Utc>>,
}

pub(super) fn capability_from_row(row: CapabilityRow) -> Result<CapabilityRecord, Error> {
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

pub(super) fn checked_configuration_count(value: i64, column: &str) -> Result<u64, Error> {
    u64::try_from(value).map_err(|_| {
        Error::Invalid(format!(
            "stored provider {column} is outside the supported range"
        ))
    })
}
