use std::{fmt, time::Duration};

use chrono::{DateTime, Utc};
use sqlx::{PgPool, Row, postgres::PgPoolOptions};
use thiserror::Error;
use uuid::Uuid;

mod idempotency;
mod runtime;
mod sessions;
mod setup;

pub use idempotency::{
    IdempotencyOutcome, IdempotencyResponse, ReplayableIdempotency, idempotency_fingerprint,
    idempotency_secret_digest,
};
pub(crate) use idempotency::{
    ReplayableIdempotencyClaim, claim_idempotency, claim_replayable_idempotency,
    complete_idempotency, complete_replayable_idempotency,
};

#[derive(Debug, Error)]
pub enum PersistenceError {
    #[error("database operation failed")]
    Database(#[from] sqlx::Error),
    #[error("database migration failed")]
    Migration(#[from] sqlx::migrate::MigrateError),
    #[error("installation setup has already completed")]
    AlreadySetup,
    #[error("runtime release failed integrity verification")]
    CorruptRelease,
    #[error("runtime snapshot is invalid: {0}")]
    InvalidRuntimeSnapshot(#[from] olp_domain::SnapshotValidationError),
    #[error("runtime release serialization failed")]
    Serialize(#[from] serde_json::Error),
    #[error("session lifetime must be positive and representable")]
    InvalidSessionTtl,
    #[error("request metadata gap is invalid")]
    InvalidRequestMetadataGap,
    #[error("request metadata event timing or status is invalid")]
    InvalidRequestMetadataEvent,
    #[error("stored {0} is outside the supported closed set")]
    InvalidStoredValue(&'static str),
    #[error("idempotency replay encryption failed")]
    IdempotencyReplayEncryption,
    #[error("idempotency replay material is unavailable or corrupt")]
    IdempotencyReplayUnavailable,
}

#[derive(Clone)]
pub struct PgStore {
    pool: PgPool,
}

impl PgStore {
    pub async fn connect(
        database_url: &str,
        max_connections: u32,
    ) -> Result<Self, PersistenceError> {
        let pool = PgPoolOptions::new()
            .max_connections(max_connections)
            .acquire_timeout(Duration::from_secs(5))
            .connect(database_url)
            .await?;
        Ok(Self { pool })
    }

    pub fn from_pool(pool: PgPool) -> Self {
        Self { pool }
    }

    pub fn pool(&self) -> &PgPool {
        &self.pool
    }

    pub async fn migrate(&self) -> Result<(), PersistenceError> {
        crate::MIGRATOR.run(&self.pool).await?;
        Ok(())
    }

    pub async fn ping(&self) -> Result<(), PersistenceError> {
        sqlx::query("SELECT 1").execute(&self.pool).await?;
        Ok(())
    }

    pub async fn setup_required(&self) -> Result<bool, PersistenceError> {
        let exists: bool = sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM installation)")
            .fetch_one(&self.pool)
            .await?;
        Ok(!exists)
    }

    pub async fn pending_outbox(&self, limit: i64) -> Result<Vec<OutboxRecord>, PersistenceError> {
        let rows = sqlx::query(
            "SELECT id, topic, aggregate_id, payload, created_at \
             FROM transactional_outbox WHERE published_at IS NULL \
             ORDER BY created_at LIMIT $1",
        )
        .bind(limit.clamp(1, 1_000))
        .fetch_all(&self.pool)
        .await?;
        Ok(rows
            .into_iter()
            .map(|row| OutboxRecord {
                id: row.get("id"),
                topic: row.get("topic"),
                aggregate_id: row.get("aggregate_id"),
                payload: row.get("payload"),
                created_at: row.get("created_at"),
            })
            .collect())
    }

    pub async fn mark_outbox_published(&self, id: Uuid) -> Result<bool, PersistenceError> {
        let result = sqlx::query(
            "UPDATE transactional_outbox SET published_at = now() \
             WHERE id = $1 AND published_at IS NULL",
        )
        .bind(id)
        .execute(&self.pool)
        .await?;
        Ok(result.rows_affected() == 1)
    }

    /// Records a gap exactly once for a durable source identity such as a
    /// Valkey Stream entry or decoded event ID. This closes the commit-before-
    /// acknowledgement crash window without storing content.
    pub async fn report_request_metadata_gap_once(
        &self,
        gap: RequestMetadataGap,
        deduplication_key: &str,
    ) -> Result<bool, PersistenceError> {
        if deduplication_key.is_empty() || deduplication_key.len() > 256 {
            return Err(PersistenceError::InvalidRequestMetadataGap);
        }
        self.insert_request_metadata_gap(gap, Some(deduplication_key))
            .await
    }

    async fn insert_request_metadata_gap(
        &self,
        gap: RequestMetadataGap,
        deduplication_key: Option<&str>,
    ) -> Result<bool, PersistenceError> {
        if gap.event_count <= 0
            || gap.gateway_instance.trim().is_empty()
            || gap.reason.trim().is_empty()
            || gap.last_observed_at < gap.first_observed_at
        {
            return Err(PersistenceError::InvalidRequestMetadataGap);
        }
        let result = sqlx::query(
            "INSERT INTO request_metadata_ingestion_gaps \
             (id, gateway_instance, event_count, reason, first_observed_at, last_observed_at, \
              deduplication_key) \
             VALUES ($1, $2, $3, $4, $5, $6, $7) \
             ON CONFLICT (deduplication_key) WHERE deduplication_key IS NOT NULL DO NOTHING",
        )
        .bind(Uuid::now_v7())
        .bind(gap.gateway_instance)
        .bind(gap.event_count)
        .bind(gap.reason)
        .bind(gap.first_observed_at)
        .bind(gap.last_observed_at)
        .bind(deduplication_key)
        .execute(&self.pool)
        .await?;
        Ok(result.rows_affected() == 1)
    }
}

pub struct NewOwner {
    pub installation_name: String,
    pub email: String,
    pub display_name: String,
    pub password_hash: String,
}

impl fmt::Debug for NewOwner {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("NewOwner")
            .field("installation_name", &self.installation_name)
            .field("email", &self.email)
            .field("display_name", &self.display_name)
            .field("password_hash", &"[REDACTED]")
            .finish()
    }
}

#[derive(Debug, Clone)]
pub struct SetupResult {
    pub user_id: Uuid,
    pub email: String,
    pub display_name: String,
    pub created_at: DateTime<Utc>,
}

#[derive(Clone)]
pub struct SessionPrincipal {
    pub session_id: Uuid,
    pub user_id: Uuid,
    pub email: String,
    pub display_name: String,
    pub role: String,
    pub csrf_digest: Vec<u8>,
    pub expires_at: DateTime<Utc>,
}

impl fmt::Debug for SessionPrincipal {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SessionPrincipal")
            .field("session_id", &self.session_id)
            .field("user_id", &self.user_id)
            .field("email", &self.email)
            .field("display_name", &self.display_name)
            .field("role", &self.role)
            .field("csrf_digest", &"[REDACTED]")
            .field("expires_at", &self.expires_at)
            .finish()
    }
}

#[derive(Clone)]
pub struct PasswordUser {
    pub id: Uuid,
    pub email: String,
    pub display_name: String,
    pub password_hash: String,
    pub role: String,
}

impl fmt::Debug for PasswordUser {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("PasswordUser")
            .field("id", &self.id)
            .field("email", &self.email)
            .field("display_name", &self.display_name)
            .field("password_hash", &"[REDACTED]")
            .field("role", &self.role)
            .finish()
    }
}

#[derive(Clone)]
pub struct PublishedRelease {
    pub generation_id: Uuid,
    pub sequence: i64,
    pub payload: Vec<u8>,
    pub sha256: [u8; 32],
    pub created_at: DateTime<Utc>,
}

impl fmt::Debug for PublishedRelease {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("PublishedRelease")
            .field("generation_id", &self.generation_id)
            .field("sequence", &self.sequence)
            .field("payload", &"[REDACTED]")
            .field("sha256", &self.sha256)
            .field("created_at", &self.created_at)
            .finish()
    }
}

#[derive(Clone)]
pub struct OutboxRecord {
    pub id: Uuid,
    pub topic: String,
    pub aggregate_id: Uuid,
    pub payload: Vec<u8>,
    pub created_at: DateTime<Utc>,
}

impl fmt::Debug for OutboxRecord {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("OutboxRecord")
            .field("id", &self.id)
            .field("topic", &self.topic)
            .field("aggregate_id", &self.aggregate_id)
            .field("payload", &"[REDACTED]")
            .field("created_at", &self.created_at)
            .finish()
    }
}

#[derive(Debug, Clone)]
pub struct RequestMetadataGap {
    pub gateway_instance: String,
    pub event_count: i64,
    pub reason: String,
    pub first_observed_at: DateTime<Utc>,
    pub last_observed_at: DateTime<Utc>,
}

#[cfg(test)]
mod tests;
