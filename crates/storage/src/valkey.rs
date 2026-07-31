//! Typed Valkey adapters for runtime hints and durable request metadata delivery.
//!
//! Redis protocol details stay inside storage; process orchestration only sees
//! runtime-hint notifications and a worker operation over [`PgStore`].

use chrono::{DateTime, Utc};
use futures::StreamExt as _;
use redis::{
    AsyncCommands,
    aio::{ConnectionManager, PubSubStream},
    streams::{StreamInfoGroupsReply, StreamPendingReply, StreamReadOptions, StreamReadReply},
};
use thiserror::Error;
use tokio::sync::watch;
use tracing::{error, warn};

use crate::{
    PersistenceError, PgStore, RequestMetadataEvent, RequestMetadataGap,
    RequestMetadataPersistenceOutcome,
    request_metadata::{MAX_REQUEST_METADATA_EVENT_BYTES, REQUEST_METADATA_TRIM_COUNTER},
};

const RUNTIME_HINT_CHANNEL: &str = "olp:v2:runtime";
pub const REQUEST_METADATA_STREAM: &str = "olp:v2:request-metadata";
const REQUEST_METADATA_GROUP: &str = "olp:persistence";
const REQUEST_METADATA_CONSUMER: &str = "worker";
const REQUEST_METADATA_DEAD_LETTER_STREAM: &str = "olp:v2:request-metadata:dead-letter";
const REQUEST_METADATA_DEAD_LETTER_MAX_ENTRIES: usize = 10_000;

pub(crate) async fn supports_hash_field_expiration(
    connection: &mut ConnectionManager,
) -> Result<bool, redis::RedisError> {
    let info: String = redis::cmd("INFO")
        .arg("server")
        .query_async(connection)
        .await?;
    Ok(info.lines().any(|line| {
        line.strip_prefix("valkey_version:")
            .is_some_and(|version| version_at_least(version, 9, 0))
            || line
                .strip_prefix("redis_version:")
                .is_some_and(|version| version_at_least(version, 7, 4))
    }))
}

fn version_at_least(version: &str, minimum_major: u64, minimum_minor: u64) -> bool {
    let mut components = version.trim().split('.');
    let Some(major) = components.next().and_then(|value| value.parse().ok()) else {
        return false;
    };
    let Some(minor) = components.next().and_then(|value| value.parse().ok()) else {
        return false;
    };
    (major, minor) >= (minimum_major, minimum_minor)
}

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

/// Runs the stable single-consumer request metadata worker until shutdown. PostgreSQL is
/// committed before an entry is acknowledged and deleted from Valkey.
pub async fn run_request_metadata_consumer(
    store: &PgStore,
    valkey_url: &str,
    mut shutdown: watch::Receiver<bool>,
) -> Result<(), ValkeyAdapterError> {
    let mut connection = valkey_connection(valkey_url).await?;
    let create: Result<String, redis::RedisError> = redis::cmd("XGROUP")
        .arg("CREATE")
        .arg(REQUEST_METADATA_STREAM)
        .arg(REQUEST_METADATA_GROUP)
        .arg("0")
        .arg("MKSTREAM")
        .query_async(&mut connection)
        .await;
    if let Err(error) = create
        && error.code() != Some("BUSYGROUP")
    {
        return Err(error.into());
    }

    let mut drain_pending = true;
    let mut last_health_checkpoint =
        tokio::time::Instant::now() - std::time::Duration::from_secs(5);
    loop {
        let id = if drain_pending { "0" } else { ">" };
        let options = StreamReadOptions::default()
            .group(REQUEST_METADATA_GROUP, REQUEST_METADATA_CONSUMER)
            .count(100)
            .block(if drain_pending { 1 } else { 1_000 });
        let streams = [REQUEST_METADATA_STREAM];
        let ids = [id];
        let reply: Option<StreamReadReply> = tokio::select! {
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return Ok(());
                }
                continue;
            }
            reply = connection.xread_options(&streams, &ids, &options) => reply?,
        };
        let entries = reply
            .into_iter()
            .flat_map(|reply| reply.keys)
            .flat_map(|stream| stream.ids)
            .collect::<Vec<_>>();
        if entries.is_empty() {
            drain_pending = false;
            if last_health_checkpoint.elapsed() >= std::time::Duration::from_secs(5) {
                checkpoint_request_metadata_consumer_health(store, &mut connection).await?;
                last_health_checkpoint = tokio::time::Instant::now();
            }
            continue;
        }

        for entry in entries {
            if entry.map.is_empty() {
                // XTRIM leaves pending-entry tombstones in the consumer group.
                // Their loss is already represented by the durable trim count.
                acknowledge_and_delete(&mut connection, &entry.id).await?;
                continue;
            }
            let decoded = decode_request_metadata_stream_event(entry.map.get("event"));
            let (payload, event) = match decoded {
                Ok(decoded) => decoded,
                Err(reason) => {
                    error!(stream_id = %entry.id, reason, "dead-lettering invalid request metadata stream event");
                    let now = Utc::now();
                    store
                        .report_request_metadata_gap_once(
                            RequestMetadataGap {
                                gateway_instance: "request-metadata-consumer".to_owned(),
                                event_count: 1,
                                reason: reason.to_owned(),
                                first_observed_at: now,
                                last_observed_at: now,
                            },
                            &format!("request-metadata-stream:{}:{reason}", entry.id),
                        )
                        .await?;
                    let payload = entry
                        .map
                        .get("event")
                        .and_then(redis_value_bytes)
                        .unwrap_or_default();
                    dead_letter_and_acknowledge(&mut connection, &entry.id, payload).await?;
                    continue;
                }
            };

            match store
                .persist_request_metadata_stream_event(&event, payload)
                .await
            {
                Ok(outcome) => {
                    if outcome == RequestMetadataPersistenceOutcome::RejectedOutsideReplayWindow {
                        warn!(stream_id = %entry.id, "request metadata event outside the replay window was recorded as an uncertain gap");
                    }
                    acknowledge_and_delete(&mut connection, &entry.id).await?;
                }
                Err(error) if is_permanent_request_metadata_error(&error) => {
                    error!(%error, stream_id = %entry.id, "dead-lettering permanently invalid request metadata event");
                    let observed_at = Utc::now();
                    store
                        .report_request_metadata_gap_once(
                            RequestMetadataGap {
                                gateway_instance: "request-metadata-consumer".to_owned(),
                                event_count: 1,
                                reason: "permanent_request_metadata_event_error".to_owned(),
                                first_observed_at: observed_at,
                                last_observed_at: observed_at,
                            },
                            &format!("request-metadata-event:{}:permanent", event.event_id),
                        )
                        .await?;
                    dead_letter_and_acknowledge(&mut connection, &entry.id, payload).await?;
                }
                Err(error) => {
                    warn!(%error, stream_id = %entry.id, "request metadata persistence will retry");
                    drain_pending = true;
                    tokio::time::sleep(std::time::Duration::from_millis(100)).await;
                    break;
                }
            }
        }
        if last_health_checkpoint.elapsed() >= std::time::Duration::from_secs(5) {
            checkpoint_request_metadata_consumer_health(store, &mut connection).await?;
            last_health_checkpoint = tokio::time::Instant::now();
        }
    }
}

fn is_permanent_request_metadata_error(error: &PersistenceError) -> bool {
    match error {
        PersistenceError::InvalidRequestMetadataEvent => true,
        PersistenceError::Database(sqlx::Error::Database(error)) => error
            .code()
            .is_some_and(|code| is_permanent_request_metadata_sqlstate(code.as_ref())),
        _ => false,
    }
}

fn is_permanent_request_metadata_sqlstate(code: &str) -> bool {
    code.starts_with("22") || code.starts_with("23")
}

async fn dead_letter_and_acknowledge(
    connection: &mut ConnectionManager,
    id: &str,
    payload: &[u8],
) -> Result<(), ValkeyAdapterError> {
    let _: String = redis::Script::new(include_str!("../scripts/dead_letter_request_metadata.lua"))
        .key(REQUEST_METADATA_STREAM)
        .key(REQUEST_METADATA_DEAD_LETTER_STREAM)
        .arg(REQUEST_METADATA_GROUP)
        .arg(id)
        .arg(payload)
        .arg(REQUEST_METADATA_DEAD_LETTER_MAX_ENTRIES)
        .invoke_async(connection)
        .await?;
    Ok(())
}

fn redis_value_bytes(value: &redis::Value) -> Option<&[u8]> {
    match value {
        redis::Value::BulkString(value) => Some(value),
        redis::Value::SimpleString(value) => Some(value.as_bytes()),
        _ => None,
    }
}

fn decode_request_metadata_stream_event(
    value: Option<&redis::Value>,
) -> Result<(&[u8], RequestMetadataEvent), &'static str> {
    match value.and_then(redis_value_bytes) {
        Some(payload) if payload.len() > MAX_REQUEST_METADATA_EVENT_BYTES => {
            Err("oversized_stream_event")
        }
        Some(payload) => serde_json::from_slice(payload)
            .map(|event| (payload, event))
            .map_err(|_| "malformed_stream_event"),
        None => Err("malformed_stream_event"),
    }
}

async fn acknowledge_and_delete(
    connection: &mut ConnectionManager,
    id: &str,
) -> Result<(), ValkeyAdapterError> {
    let _: (usize, usize) = redis::pipe()
        .atomic()
        .cmd("XACK")
        .arg(REQUEST_METADATA_STREAM)
        .arg(REQUEST_METADATA_GROUP)
        .arg(id)
        .cmd("XDEL")
        .arg(REQUEST_METADATA_STREAM)
        .arg(id)
        .query_async(connection)
        .await?;
    Ok(())
}

async fn checkpoint_request_metadata_consumer_health(
    store: &PgStore,
    connection: &mut ConnectionManager,
) -> Result<(), ValkeyAdapterError> {
    let trimmed: Option<u64> = redis::cmd("GET")
        .arg(REQUEST_METADATA_TRIM_COUNTER)
        .query_async(connection)
        .await?;
    if let Some(trimmed) = trimmed {
        store
            .report_request_metadata_stream_trim_uncertainty(trimmed)
            .await?;
    }
    let pending: StreamPendingReply = connection
        .xpending(REQUEST_METADATA_STREAM, REQUEST_METADATA_GROUP)
        .await?;
    let (pending_events, oldest_pending_at) = match pending {
        StreamPendingReply::Empty => (0_u64, None),
        StreamPendingReply::Data(data) => {
            let timestamp = data
                .start_id
                .split_once('-')
                .and_then(|(millis, _)| millis.parse::<i64>().ok())
                .and_then(DateTime::<Utc>::from_timestamp_millis)
                .ok_or(ValkeyAdapterError::InvalidState(
                    "invalid pending stream ID",
                ))?;
            let count = u64::try_from(data.count)
                .map_err(|_| ValkeyAdapterError::InvalidState("pending count overflow"))?;
            (count, Some(timestamp))
        }
        _ => {
            return Err(ValkeyAdapterError::InvalidState(
                "unrecognized pending stream reply",
            ));
        }
    };
    let groups: StreamInfoGroupsReply = connection.xinfo_groups(REQUEST_METADATA_STREAM).await?;
    let group = groups
        .groups
        .into_iter()
        .find(|candidate| candidate.name == REQUEST_METADATA_GROUP)
        .ok_or(ValkeyAdapterError::InvalidState(
            "consumer group disappeared",
        ))?;
    let Some(lag) = group.lag else {
        warn!("request metadata stream lag is unknown; retaining the previous health checkpoint");
        return Ok(());
    };
    let lag_events =
        u64::try_from(lag).map_err(|_| ValkeyAdapterError::InvalidState("stream lag overflow"))?;
    store
        .report_request_metadata_consumer_health(pending_events, lag_events, oldest_pending_at)
        .await?;
    Ok(())
}

async fn valkey_connection(url: &str) -> Result<ConnectionManager, redis::RedisError> {
    let client = redis::Client::open(url)?;
    ConnectionManager::new(client).await
}

#[cfg(test)]
mod tests {
    use super::{
        MAX_REQUEST_METADATA_EVENT_BYTES, decode_request_metadata_stream_event,
        is_permanent_request_metadata_error, is_permanent_request_metadata_sqlstate,
        version_at_least,
    };
    use crate::PersistenceError;

    #[test]
    fn request_metadata_dead_letters_only_event_local_failures() {
        for code in ["22003", "22007", "23503", "23514"] {
            assert!(
                is_permanent_request_metadata_sqlstate(code),
                "{code} is caused by event data"
            );
        }
        for code in [
            "08006", "40001", "40P01", "42501", "42P01", "53300", "57014", "58030", "XX000",
        ] {
            assert!(
                !is_permanent_request_metadata_sqlstate(code),
                "{code} must leave the source event pending for retry"
            );
        }
        assert!(!is_permanent_request_metadata_error(
            &PersistenceError::Database(sqlx::Error::PoolTimedOut)
        ));
        assert!(!is_permanent_request_metadata_error(
            &PersistenceError::Database(sqlx::Error::RowNotFound)
        ));
        assert!(is_permanent_request_metadata_error(
            &PersistenceError::InvalidRequestMetadataEvent
        ));
    }

    #[test]
    fn oversized_request_metadata_is_rejected_before_deserialization() {
        let payload = redis::Value::BulkString(vec![b'{'; MAX_REQUEST_METADATA_EVENT_BYTES + 1]);
        assert!(matches!(
            decode_request_metadata_stream_event(Some(&payload)),
            Err("oversized_stream_event")
        ));
    }

    #[test]
    fn hash_field_expiration_version_floors_are_explicit() {
        assert!(version_at_least("9.0.0", 9, 0));
        assert!(version_at_least("7.4.1\r", 7, 4));
        assert!(!version_at_least("8.1.0", 9, 0));
        assert!(!version_at_least("7.2.7", 7, 4));
    }
}
