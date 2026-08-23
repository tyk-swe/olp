//! Typed Valkey adapters for runtime hints and durable request metadata delivery.
//!
//! Redis protocol details stay inside storage; process orchestration only sees
//! runtime-hint notifications and a worker operation over [`Store`].

use futures::StreamExt as _;
use redis::aio::{ConnectionManager, PubSubStream};
use thiserror::Error;
use uuid::Uuid;

use crate::{error::Error as PersistenceError, store::Store};
use request_metadata::LEGACY_REQUEST_METADATA_STREAM;

pub mod request_metadata;

const KEYSPACE_VERSION: &str = "olp:v3";
const INSTALLATION_IDENTITY_MIGRATION_VERSION: i64 = 32;
const LEGACY_REQUEST_METADATA_STREAM_CLAIM_KEY: &str = "olp:v2:request-metadata:migration-claim";

/// Installation-scoped Valkey resource names derived from PostgreSQL's durable
/// identity. Restoring PostgreSQL therefore restores the same Valkey namespace.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Keyspace {
    prefix: String,
}

impl Keyspace {
    #[must_use]
    fn from_installation_id(id: Uuid) -> Self {
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

impl Store {
    /// Returns true only when the pre-migration database history proves this is
    /// an upgrade from a schema that predates installation-scoped Valkey names.
    pub async fn should_claim_legacy_request_metadata_stream(
        &self,
    ) -> Result<bool, PersistenceError> {
        let migration_history_exists: bool = sqlx::query_scalar!(
            "SELECT to_regclass('public._sqlx_migrations') IS NOT NULL AS \"exists!\""
        )
        .fetch_one(self.pool())
        .await?;
        if !migration_history_exists {
            return Ok(false);
        }

        let latest_successful_migration: Option<i64> = sqlx::query_scalar!(
            "SELECT max(version) AS \"version\" FROM public._sqlx_migrations WHERE success"
        )
        .fetch_one(self.pool())
        .await?;
        Ok(latest_successful_migration
            .is_some_and(|version| version < INSTALLATION_IDENTITY_MIGRATION_VERSION))
    }

    pub async fn valkey_keyspace(&self) -> Result<Keyspace, PersistenceError> {
        let id = sqlx::query_scalar!("SELECT id FROM installation_identity WHERE singleton")
            .fetch_one(self.pool())
            .await?;
        Ok(Keyspace::from_installation_id(id))
    }
}

#[derive(Debug, Error)]
pub enum Error {
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

/// Marks this database as the owner of a later legacy stream move before SQL
/// migration 0032 can make the upgrade look like a current-schema database.
pub async fn mark_legacy_request_metadata_stream_claim(
    url: &str,
    claim_token: &str,
) -> Result<bool, Error> {
    let mut connection = valkey_connection(url).await?;
    let script = redis::Script::new(
        "local legacy = redis.call('EXISTS', KEYS[1])\n\
         if legacy == 0 then\n\
           redis.call('DEL', KEYS[2])\n\
           return 0\n\
         end\n\
         local claim = redis.call('GET', KEYS[2])\n\
         if claim and claim ~= ARGV[1] then\n\
           return redis.error_reply('legacy request metadata stream is already claimed by another database migration')\n\
         end\n\
         redis.call('SET', KEYS[2], ARGV[1])\n\
         return 1",
    );
    let claimed: i64 = script
        .key(LEGACY_REQUEST_METADATA_STREAM)
        .key(LEGACY_REQUEST_METADATA_STREAM_CLAIM_KEY)
        .arg(claim_token)
        .invoke_async(&mut connection)
        .await?;
    Ok(claimed == 1)
}

/// Atomically moves a stream that was marked by this database before SQL
/// migration 0032 completed. A post-0032 retry can safely finish only when the
/// pre-SQL claim token still matches this database.
pub async fn migrate_claimed_legacy_request_metadata_stream(
    url: &str,
    target_stream: &str,
    claim_token: &str,
) -> Result<bool, Error> {
    let mut connection = valkey_connection(url).await?;
    let script = redis::Script::new(
        "local legacy = redis.call('EXISTS', KEYS[1])\n\
         local claim = redis.call('GET', KEYS[3])\n\
         if legacy == 0 then\n\
           redis.call('DEL', KEYS[3])\n\
           return 0\n\
         end\n\
         if not claim then return 0 end\n\
         if claim ~= ARGV[1] then\n\
           return redis.error_reply('legacy request metadata stream is claimed by another database migration')\n\
         end\n\
         if redis.call('EXISTS', KEYS[2]) ~= 0 then\n\
           return redis.error_reply('legacy and installation-scoped request metadata streams both exist')\n\
         end\n\
         redis.call('RENAME', KEYS[1], KEYS[2])\n\
         redis.call('DEL', KEYS[3])\n\
         return 1",
    );
    let migrated: i64 = script
        .key(LEGACY_REQUEST_METADATA_STREAM)
        .key(target_stream)
        .key(LEGACY_REQUEST_METADATA_STREAM_CLAIM_KEY)
        .arg(claim_token)
        .invoke_async(&mut connection)
        .await?;
    Ok(migrated == 1)
}

/// Verifies that migration has removed the only pre-namespace Valkey resource.
pub async fn verify_request_metadata_stream_upgrade(url: &str) -> Result<(), Error> {
    let mut connection = valkey_connection(url).await?;
    let legacy_exists: bool = redis::cmd("EXISTS")
        .arg(LEGACY_REQUEST_METADATA_STREAM)
        .query_async(&mut connection)
        .await?;
    if legacy_exists {
        return Err(Error::InvalidState(
            "legacy request metadata stream still exists; stop N-1 processes and use only the pre-0032 database that owns that stream to claim it",
        ));
    }
    Ok(())
}

pub(super) async fn valkey_connection(url: &str) -> Result<ConnectionManager, redis::RedisError> {
    let client = redis::Client::open(url)?;
    ConnectionManager::new(client).await
}

#[cfg(test)]
mod tests {
    use super::Keyspace;

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
            "olp:v3:00000000-0000-0000-0000-000000000001:request-metadata"
        );
        assert_ne!(first.runtime_hint_channel(), second.runtime_hint_channel());
        assert_ne!(first.limits_namespace(), second.limits_namespace());
    }
}
