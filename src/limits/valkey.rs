//! Typed Valkey adapters for runtime hints and durable request metadata delivery.
//!
//! Redis protocol details stay inside storage; process orchestration only sees
//! runtime-hint notifications and a worker operation over [`PgPool`].

use futures::StreamExt as _;
use redis::aio::ConnectionManager;
use redis::aio::PubSubStream;
use thiserror::Error;
use uuid::Uuid;

use crate::database::error::Error as PersistenceError;

const KEYSPACE_VERSION: &str = "olp:3";

/// Installation-scoped Valkey resource names derived from PostgreSQL's durable
/// identity. Restoring PostgreSQL therefore restores the same Valkey namespace.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Keyspace {
    prefix: String,
}

impl Keyspace {
    #[must_use]
    pub fn from_installation_id(id: Uuid) -> Self {
        Self {
            prefix: format!("{KEYSPACE_VERSION}:{id}"),
        }
    }

    #[must_use]
    pub fn prefix(&self) -> &str {
        &self.prefix
    }

    #[must_use]
    pub fn runtime_hint_channel(&self) -> String {
        format!("{}:runtime", self.prefix)
    }

    #[must_use]
    pub fn request_metadata_stream(&self) -> String {
        format!("{}:request-metadata", self.prefix)
    }

    #[must_use]
    pub fn limits_namespace(&self) -> String {
        format!("{}:limits", self.prefix)
    }
}

pub async fn valkey_keyspace(pool: &sqlx::PgPool) -> Result<Keyspace, PersistenceError> {
    Ok(Keyspace::from_installation_id(
        crate::access::identity::installation::installation_id(pool).await?,
    ))
}

#[derive(Debug, Error)]
pub enum Error {
    #[error("Valkey operation failed")]
    Service(#[from] redis::RedisError),
    #[error("storage operation failed")]
    Storage(#[from] PersistenceError),
    #[error("cost limiter operation failed")]
    CostLimiter(#[from] crate::limits::admission::LimitError),
    #[error("Valkey returned invalid stream state: {0}")]
    InvalidState(&'static str),
}

/// An owned runtime-hint stream. Message payloads are deliberately hidden:
/// hints only trigger an authoritative PostgreSQL release read.
pub struct RuntimeHintSubscriber {
    messages: PubSubStream,
}

impl RuntimeHintSubscriber {
    pub async fn connect(url: &str, channel: &str) -> Result<Self, Error> {
        let client = redis::Client::open(url)?;
        let mut pubsub = client.get_async_pubsub().await?;
        pubsub.subscribe(channel).await?;
        Ok(Self {
            messages: pubsub.into_on_message(),
        })
    }

    pub async fn recv(&mut self) -> Result<(), Error> {
        self.messages
            .next()
            .await
            .map(|_| ())
            .ok_or(Error::InvalidState("runtime hint subscription ended"))
    }
}

/// Typed publisher for the transactional runtime-release outbox.
pub struct RuntimeHintPublisher {
    connection: ConnectionManager,
    channel: String,
}

impl RuntimeHintPublisher {
    pub async fn connect(url: &str, channel: &str) -> Result<Self, Error> {
        Ok(Self {
            connection: valkey_connection(url).await?,
            channel: channel.to_owned(),
        })
    }

    pub async fn publish(&mut self, payload: &[u8]) -> Result<u64, Error> {
        let subscribers: i64 = redis::cmd("PUBLISH")
            .arg(&self.channel)
            .arg(payload)
            .query_async(&mut self.connection)
            .await?;
        u64::try_from(subscribers).map_err(|_| Error::InvalidState("negative subscriber count"))
    }
}

pub(crate) async fn valkey_connection(url: &str) -> Result<ConnectionManager, redis::RedisError> {
    let client = redis::Client::open(url)?;
    ConnectionManager::new(client).await
}

#[cfg(test)]
mod tests {
    use crate::limits::valkey::Keyspace;

    #[test]
    fn installation_keyspaces_are_disjoint_and_stable() {
        let first = Keyspace::from_installation_id(
            uuid::Uuid::parse_str("00000000-0000-0000-0000-000000000001").unwrap(),
        );
        let second = Keyspace::from_installation_id(
            uuid::Uuid::parse_str("00000000-0000-0000-0000-000000000002").unwrap(),
        );
        assert_eq!(
            first.request_metadata_stream(),
            "olp:3:00000000-0000-0000-0000-000000000001:request-metadata"
        );
        assert_ne!(first.runtime_hint_channel(), second.runtime_hint_channel());
        assert_ne!(first.limits_namespace(), second.limits_namespace());
    }
}
