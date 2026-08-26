use chrono::{DateTime, Utc};
use uuid::Uuid;

use super::cursor::Error;
use crate::audit_events::{AuditEvent, record_audit_event};
use crate::store::Store;

#[derive(Clone, Debug)]
pub struct SettingRecord {
    pub key: String,
    pub value: String,
    pub etag: Uuid,
    pub updated_by: Uuid,
    pub updated_at: DateTime<Utc>,
}

impl Store {
    pub async fn settings(&self) -> Result<Vec<SettingRecord>, Error> {
        let rows = sqlx::query!(
            "SELECT key, value, etag, updated_by, updated_at FROM settings ORDER BY key",
        )
        .fetch_all(self.pool())
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

    pub async fn update_setting(
        &self,
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
        let mut transaction = self.pool().begin().await?;
        let etag = Uuid::now_v7();
        let now = Utc::now();
        let row = sqlx::query!(
            "UPDATE settings SET value = $1, etag = $2, updated_by = $3, updated_at = $4 \
             WHERE key = $5 AND etag = $6 \
             RETURNING key, value, etag, updated_by, updated_at",
            value,
            etag,
            actor,
            now,
            key,
            expected_etag
        )
        .fetch_optional(&mut *transaction)
        .await?;
        let Some(row) = row else {
            let exists: bool = sqlx::query_scalar!(
                "SELECT EXISTS (SELECT 1 FROM settings WHERE key = $1) AS \"value!\"",
                key
            )
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
                provenance: self.provenance(),
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
}
