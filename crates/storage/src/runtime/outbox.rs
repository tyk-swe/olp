use uuid::Uuid;

use crate::{PersistenceError, PgStore};

use super::OutboxRecord;

impl PgStore {
    pub async fn pending_outbox(&self, limit: i64) -> Result<Vec<OutboxRecord>, PersistenceError> {
        let rows = sqlx::query!(
            "SELECT id, topic, aggregate_id, payload, created_at \
             FROM transactional_outbox WHERE published_at IS NULL \
             ORDER BY created_at LIMIT $1",
            limit.clamp(1, 1_000)
        )
        .fetch_all(self.pool())
        .await?;
        Ok(rows
            .into_iter()
            .map(|row| OutboxRecord {
                id: row.id,
                topic: row.topic,
                aggregate_id: row.aggregate_id,
                payload: row.payload,
                created_at: row.created_at,
            })
            .collect())
    }

    pub async fn mark_outbox_published(&self, id: Uuid) -> Result<bool, PersistenceError> {
        let result = sqlx::query!(
            "UPDATE transactional_outbox SET published_at = now() \
             WHERE id = $1 AND published_at IS NULL",
            id
        )
        .execute(self.pool())
        .await?;
        Ok(result.rows_affected() == 1)
    }
}
