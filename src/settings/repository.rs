use chrono::DateTime;
use chrono::Utc;
use uuid::Uuid;

use crate::access::audit_events::AuditEvent;
use crate::access::audit_events::record_audit_event;
use crate::database::cursor::Error;

pub const LIMITS_VALKEY_UNAVAILABLE_KEY: &str = "limits.valkey_unavailable";

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum LimitsValkeyUnavailablePolicy {
    #[default]
    FailClosed,
    FailOpen,
}

impl LimitsValkeyUnavailablePolicy {
    #[must_use]
    pub fn parse(value: &str) -> Option<Self> {
        match value {
            "fail_closed" => Some(Self::FailClosed),
            "fail_open" => Some(Self::FailOpen),
            _ => None,
        }
    }
}

#[derive(Clone, Debug)]
pub struct SettingRecord {
    pub key: String,
    pub value: String,
    pub etag: Uuid,
    pub updated_by: Uuid,
    pub updated_at: DateTime<Utc>,
}

pub async fn settings(pool: &sqlx::PgPool) -> Result<Vec<SettingRecord>, Error> {
    let rows = sqlx::query_as::<_, SettingsRow>(
        "SELECT key, value, etag, updated_by, updated_at FROM settings ORDER BY key",
    )
    .fetch_all(pool)
    .await?;
    Ok(rows
        .into_iter()
        .map(|row| SettingRecord {
            key: row.key,
            value: row.value,
            etag: row.etag,
            updated_by: row.updated_by,
            updated_at: row.updated_at,
        })
        .collect())
}

pub async fn limits_valkey_unavailable_policy(
    pool: &sqlx::PgPool,
) -> Result<LimitsValkeyUnavailablePolicy, Error> {
    let value = sqlx::query_scalar::<_, String>("SELECT value FROM settings WHERE key = $1")
        .bind(LIMITS_VALKEY_UNAVAILABLE_KEY)
        .fetch_optional(pool)
        .await?;
    Ok(value
        .as_deref()
        .and_then(LimitsValkeyUnavailablePolicy::parse)
        .unwrap_or_default())
}

pub async fn update_setting(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    key: &str,
    value: &str,
    expected_etag: Uuid,
    actor: Uuid,
) -> Result<SettingRecord, Error> {
    if key.trim().is_empty() || key.len() > 100 || value.len() > 4_096 {
        return Err(Error::Invalid(
            "setting key or value exceeds its limit".to_owned(),
        ));
    }
    if matches!(
        key,
        "retention.requests_days" | "retention.usage_days" | "retention.audit_days"
    ) && value
        .parse::<i64>()
        .ok()
        .is_none_or(|days| !(1..=3_650).contains(&days))
    {
        return Err(Error::Invalid(
            "retention days must be an integer between 1 and 3650".to_owned(),
        ));
    }
    if key == LIMITS_VALKEY_UNAVAILABLE_KEY && LimitsValkeyUnavailablePolicy::parse(value).is_none()
    {
        return Err(Error::Invalid(
            "limits.valkey_unavailable must be fail_closed or fail_open".to_owned(),
        ));
    }
    let mut transaction = pool.begin().await?;
    let etag = Uuid::now_v7();
    let now = Utc::now();
    let row = sqlx::query_as::<_, SettingsRow>(
        "UPDATE settings SET value = $1, etag = $2, updated_by = $3, updated_at = $4 \
             WHERE key = $5 AND etag = $6 \
             RETURNING key, value, etag, updated_by, updated_at",
    )
    .bind(value)
    .bind(etag)
    .bind(actor)
    .bind(now)
    .bind(key)
    .bind(expected_etag)
    .fetch_optional(&mut *transaction)
    .await?;
    let Some(row) = row else {
        let exists: bool = sqlx::query_scalar::<_, bool>(
            "SELECT EXISTS (SELECT 1 FROM settings WHERE key = $1) AS \"value\"",
        )
        .bind(key)
        .fetch_one(&mut *transaction)
        .await?;
        return Err(if exists {
            Error::PreconditionFailed
        } else {
            Error::NotFound
        });
    };
    record_audit_event(
        &mut *transaction,
        AuditEvent {
            provenance,
            actor: Some(actor),
            action: "setting.update",
            resource_type: "setting",
            resource_id: Some(key),
            outcome: "success",
            occurred_at: Some(now),
        },
    )
    .await?;
    transaction.commit().await?;
    Ok(SettingRecord {
        key: row.key,
        value: row.value,
        etag: row.etag,
        updated_by: row.updated_by,
        updated_at: row.updated_at,
    })
}

#[derive(sqlx::FromRow)]
struct SettingsRow {
    key: String,
    value: String,
    etag: uuid::Uuid,
    updated_by: uuid::Uuid,
    updated_at: chrono::DateTime<chrono::Utc>,
}
