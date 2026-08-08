//! Typed Valkey adapters for runtime hints and durable request metadata delivery.
//!
//! Redis protocol details stay inside storage; process orchestration only sees
//! runtime-hint notifications and a worker operation over [`PgStore`].

use futures::StreamExt as _;
use redis::aio::{ConnectionManager, PubSubStream};
use thiserror::Error;

use crate::PersistenceError;

mod request_metadata;

#[cfg(all(feature = "test-util", debug_assertions))]
pub use request_metadata::test_support as request_metadata_test_support;
pub use request_metadata::{
    REQUEST_METADATA_RECLAIM_IDLE, REQUEST_METADATA_RECOVERY_INTERVAL, REQUEST_METADATA_STREAM,
    run_request_metadata_consumer,
};

const RUNTIME_HINT_CHANNEL: &str = "olp:v2:runtime";

#[derive(Debug, Error)]
pub enum ValkeyAdapterError {
    #[error("Valkey operation failed")]
    Service(#[from] redis::RedisError),
    #[error("storage operation failed")]
    Storage(#[from] PersistenceError),
    #[error("Valkey returned invalid stream state: {0}")]
    InvalidState(&'static str),
}

/// An owned runtime-hint stream. Message payloads are deliberately hidden:
/// hints only trigger an authoritative PostgreSQL release read.
pub struct RuntimeHintSubscriber {
    messages: PubSubStream,
}

impl RuntimeHintSubscriber {
    pub async fn connect(url: &str) -> Result<Self, ValkeyAdapterError> {
        let client = redis::Client::open(url)?;
        let mut pubsub = client.get_async_pubsub().await?;
        pubsub.subscribe(RUNTIME_HINT_CHANNEL).await?;
        Ok(Self {
            messages: pubsub.into_on_message(),
        })
    }

    pub async fn recv(&mut self) -> Result<(), ValkeyAdapterError> {
        self.messages
            .next()
            .await
            .map(|_| ())
            .ok_or(ValkeyAdapterError::InvalidState(
                "runtime hint subscription ended",
            ))
    }
}

/// Typed publisher for the transactional runtime-release outbox.
pub struct RuntimeHintPublisher {
    connection: ConnectionManager,
}

impl RuntimeHintPublisher {
    pub async fn connect(url: &str) -> Result<Self, ValkeyAdapterError> {
        Ok(Self {
            connection: valkey_connection(url).await?,
        })
    }

    pub async fn publish(&mut self, payload: &[u8]) -> Result<u64, ValkeyAdapterError> {
        let subscribers: i64 = redis::cmd("PUBLISH")
            .arg(RUNTIME_HINT_CHANNEL)
            .arg(payload)
            .query_async(&mut self.connection)
            .await?;
        u64::try_from(subscribers)
            .map_err(|_| ValkeyAdapterError::InvalidState("negative subscriber count"))
    }
}

pub(super) async fn valkey_connection(url: &str) -> Result<ConnectionManager, redis::RedisError> {
    let client = redis::Client::open(url)?;
    ConnectionManager::new(client).await
}
