use uuid::Uuid;

use sqlx::{Connection as _, PgConnection};

use crate::{PersistenceError, PgStore};

use super::OutboxRecord;

// A session-level lock is deliberately distinct from the transaction-level
// runtime compilation lock. The numeric value is the ASCII bytes "OLP_OBX".
const OUTBOX_LEADER_LOCK_ID: i64 = 0x004f_4c50_5f4f_4258;

/// Exclusive ownership of runtime-hint outbox publication.
///
/// The lock and every read/completion query use this exact PostgreSQL session.
/// It is detached from the pool before lock acquisition, so cancellation,
/// panic, and ordinary drop close the physical connection instead of ever
/// returning a possibly locked session to the pool.
pub struct RuntimeOutboxLeader {
    connection: PgConnection,
}

impl PgStore {
    pub async fn acquire_runtime_outbox_leader(
        &self,
    ) -> Result<RuntimeOutboxLeader, PersistenceError> {
        let mut connection = self.pool().acquire().await?.detach();
        // Contenders wait in PostgreSQL instead of opening and closing a new
        // session on every poll. The raw connection cannot return to the pool,
        // including when this await is cancelled after an ambiguous acquire.
        sqlx::query!("SELECT pg_advisory_lock($1)", OUTBOX_LEADER_LOCK_ID)
            .execute(&mut connection)
            .await?;
        Ok(RuntimeOutboxLeader { connection })
    }
}

impl RuntimeOutboxLeader {
    pub async fn pending(&mut self, limit: u16) -> Result<Vec<OutboxRecord>, PersistenceError> {
        let rows = sqlx::query!(
            "SELECT id, topic, aggregate_id, payload, created_at \
             FROM transactional_outbox WHERE published_at IS NULL \
             ORDER BY created_at, id LIMIT $1",
            i64::from(limit.clamp(1, 1_000))
        )
        .fetch_all(&mut self.connection)
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

    /// Marks a successful publish through the same live session that owns the
    /// advisory lock. SQLx PostgreSQL connections do not reconnect in place:
    /// once that session is lost, this completion cannot reach PostgreSQL and
    /// a replacement leader may safely retry the still-unpublished row.
    pub async fn mark_published(&mut self, id: Uuid) -> Result<bool, PersistenceError> {
        let result = sqlx::query!(
            "UPDATE transactional_outbox SET published_at = now() \
             WHERE id = $1 AND published_at IS NULL",
            id
        )
        .execute(&mut self.connection)
        .await?;
        Ok(result.rows_affected() == 1)
    }

    /// Releases leadership on clean shutdown and closes the physical session.
    /// Error and panic paths drop the detached connection and close its socket.
    pub async fn release(mut self) -> Result<(), PersistenceError> {
        let released = sqlx::query_scalar!(
            "SELECT pg_advisory_unlock($1) AS \"released!\"",
            OUTBOX_LEADER_LOCK_ID
        )
        .fetch_one(&mut self.connection)
        .await?;
        if !released {
            return Err(PersistenceError::RuntimeOutboxLeadershipLost);
        }
        self.connection.close().await?;
        Ok(())
    }

    #[cfg(feature = "test-util")]
    pub async fn backend_pid(&mut self) -> Result<i32, PersistenceError> {
        Ok(sqlx::query_scalar!("SELECT pg_backend_pid() AS \"pid!\"")
            .fetch_one(&mut self.connection)
            .await?)
    }
}
