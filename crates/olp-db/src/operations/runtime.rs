use chrono::{DateTime, Utc};
use uuid::Uuid;

use super::{
    MAX_PAGE_SIZE,
    cursor::{OperationsError, OperationsPage, checked_u64},
};
use crate::{PgStore, split_page};

#[derive(Clone, Debug)]
pub struct RuntimeGenerationRecord {
    pub id: Uuid,
    pub sequence: u64,
    pub sha256_hex: String,
    pub created_by: Uuid,
    pub created_by_email: String,
    pub created_at: DateTime<Utc>,
}

impl PgStore {
    pub async fn runtime_generations(
        &self,
        before_sequence: Option<u64>,
        limit: u16,
    ) -> Result<OperationsPage<RuntimeGenerationRecord>, OperationsError> {
        let page_size = limit.clamp(1, MAX_PAGE_SIZE);
        let before = before_sequence
            .map(i64::try_from)
            .transpose()
            .map_err(|_| OperationsError::InvalidCursor)?;
        let rows = sqlx::query!(
            "SELECT g.id, g.sequence, encode(g.release_sha256, 'hex') AS \"sha256_hex!\", \
                    g.created_by, u.email AS created_by_email, g.created_at \
             FROM runtime_generations g JOIN users u ON u.id = g.created_by \
             WHERE ($1::bigint IS NULL OR g.sequence < $1) \
             ORDER BY g.sequence DESC LIMIT $2",
            before,
            i64::from(page_size) + 1
        )
        .fetch_all(self.pool())
        .await?;
        let items = rows
            .into_iter()
            .map(|row| {
                Ok(RuntimeGenerationRecord {
                    id: row.id,
                    sequence: checked_u64(row.sequence, "generation sequence")?,
                    sha256_hex: row.sha256_hex,
                    created_by: row.created_by,
                    created_by_email: row.created_by_email,
                    created_at: row.created_at,
                })
            })
            .collect::<Result<Vec<_>, OperationsError>>()?;
        let (items, next_cursor) = split_page(items, usize::from(page_size), |item| {
            item.sequence.to_string()
        });
        Ok(OperationsPage { items, next_cursor })
    }
}
