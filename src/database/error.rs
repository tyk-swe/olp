use thiserror::Error;

#[derive(Debug, Error)]
pub enum Error {
    #[error("OpenLLMProxy 3.0 requires a fresh database; existing 2.x storage cannot be upgraded")]
    LegacyInstallation,
    #[error("database operation failed")]
    Database(#[from] sqlx::Error),
    #[error("database migration failed")]
    Migration(#[from] sqlx::migrate::MigrateError),
    #[error("installation setup has already completed")]
    AlreadySetup,
    #[error("runtime release failed integrity verification")]
    CorruptRelease,
    #[error("runtime release serialization failed")]
    Serialize(#[from] serde_json::Error),
    #[error("runtime outbox leadership was lost")]
    RuntimeOutboxLeadershipLost,
    #[error("worker health checkpoint is invalid")]
    InvalidWorkerHealth,
    #[error("session lifetime must be positive and representable")]
    InvalidSessionTtl,
    #[error("recent-authentication metadata is invalid")]
    InvalidRecentAuthentication,
    #[error("a session cannot be created for the requested user")]
    SessionUnavailable,
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
