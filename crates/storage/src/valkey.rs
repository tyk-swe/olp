//! Typed Valkey adapters for runtime hints and durable request metadata delivery.
//!
//! Redis protocol details stay inside storage; process orchestration only sees
//! runtime-hint notifications and a worker operation over [`PgStore`].

use chrono::{DateTime, Utc};
use futures::StreamExt as _;
use redis::{
    AsyncCommands,
    aio::{ConnectionManager, ConnectionManagerConfig, PubSubStream},
    streams::{StreamInfoGroupsReply, StreamPendingReply, StreamReadOptions, StreamReadReply},
};
use thiserror::Error;
use tokio::sync::watch;
use tracing::{error, warn};

use crate::{
    PersistenceError, PgStore, RequestMetadataEvent, RequestMetadataGap,
    RequestMetadataPersistenceOutcome,
};

const RUNTIME_HINT_CHANNEL: &str = "olp:v2:runtime";
pub const REQUEST_METADATA_STREAM: &str = "olp:v2:request-metadata";
const REQUEST_METADATA_GROUP: &str = "olp:persistence";
const REQUEST_METADATA_CONSUMER: &str = "worker";
const REQUEST_METADATA_RESPONSE_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(2);

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
    let client = redis::Client::open(valkey_url)?;
    let mut connection = ConnectionManager::new_with_config(
        client,
        ConnectionManagerConfig::new()
            .set_response_timeout(Some(REQUEST_METADATA_RESPONSE_TIMEOUT)),
    )
    .await?;
    let create: Result<String, redis::RedisError> = redis::cmd("XGROUP")
        .arg("CREATE")
        .arg(REQUEST_METADATA_STREAM)
        .arg(REQUEST_METADATA_GROUP)
        .arg("0")
        .arg("MKSTREAM")
        .query_async(&mut connection)
        .await;
    if let Err(error) = create
        && !is_busygroup(&error)
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
            let payload = entry
                .map
                .get("event")
                .and_then(|value| redis::from_redis_value_ref::<String>(value).ok());
            let event = payload
                .as_deref()
                .and_then(|payload| serde_json::from_str::<RequestMetadataEvent>(payload).ok());
            let Some(event) = event else {
                error!(stream_id = %entry.id, "discarding malformed request metadata stream event");
                let now = Utc::now();
                store
                    .report_request_metadata_gap_once(
                        RequestMetadataGap {
                            gateway_instance: "request-metadata-consumer".to_owned(),
                            event_count: 1,
                            reason: "malformed_stream_event".to_owned(),
                            first_observed_at: now,
                            last_observed_at: now,
                        },
                        &format!("request-metadata-stream:{}:malformed", entry.id),
                    )
                    .await?;
                acknowledge_and_delete(&mut connection, &entry.id).await?;
                continue;
            };

            match store
                .persist_request_metadata_stream_event(
                    &event,
                    payload
                        .as_deref()
                        .expect("a decoded request metadata event has its original payload")
                        .as_bytes(),
                )
                .await
            {
                Ok(outcome) => {
                    if outcome == RequestMetadataPersistenceOutcome::RejectedOutsideReplayWindow {
                        warn!(stream_id = %entry.id, "request metadata event outside the replay window was recorded as an uncertain gap");
                    }
                    acknowledge_and_delete(&mut connection, &entry.id).await?;
                }
                Err(error) if is_permanent_request_metadata_error(&error) => {
                    error!(%error, stream_id = %entry.id, "discarding permanently invalid request metadata event");
                    let observed_at = Utc::now();
                    store
                        .report_request_metadata_gap_once(
                            RequestMetadataGap {
                                gateway_instance: "request-metadata-consumer".to_owned(),
                                event_count: 1,
                                reason: "invalid_request_metadata_event".to_owned(),
                                first_observed_at: observed_at,
                                last_observed_at: observed_at,
                            },
                            &format!("request-metadata-event:{}:invalid", event.event_id),
                        )
                        .await?;
                    acknowledge_and_delete(&mut connection, &entry.id).await?;
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
    let Some(lag_events) = checked_stream_lag(group.lag)? else {
        warn!("request metadata stream lag is unavailable; leaving consumer health stale");
        return Ok(());
    };
    store
        .report_request_metadata_consumer_health(pending_events, lag_events, oldest_pending_at)
        .await?;
    Ok(())
}

fn is_busygroup(error: &redis::RedisError) -> bool {
    error.code() == Some("BUSYGROUP")
}

fn is_permanent_request_metadata_error(error: &PersistenceError) -> bool {
    match error {
        PersistenceError::InvalidRequestMetadataEvent => true,
        PersistenceError::Database(sqlx::Error::Database(error)) => error
            .code()
            .is_some_and(|code| is_permanent_request_metadata_sqlstate(&code)),
        _ => false,
    }
}

fn is_permanent_request_metadata_sqlstate(code: &str) -> bool {
    code.starts_with("22") || code.starts_with("23")
}

fn checked_stream_lag(lag: Option<usize>) -> Result<Option<u64>, ValkeyAdapterError> {
    lag.map(u64::try_from)
        .transpose()
        .map_err(|_| ValkeyAdapterError::InvalidState("stream lag overflow"))
}

async fn valkey_connection(url: &str) -> Result<ConnectionManager, redis::RedisError> {
    let client = redis::Client::open(url)?;
    ConnectionManager::new(client).await
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn classifies_structured_group_and_permanent_database_errors() {
        let value =
            redis::parse_redis_value(b"-BUSYGROUP Consumer Group name already exists\r\n").unwrap();
        let redis::Value::ServerError(error) = value else {
            panic!("expected a server error");
        };
        let error = redis::RedisError::from(error);
        assert!(is_busygroup(&error));

        for code in ["22003", "22P02", "23503", "23514"] {
            assert!(is_permanent_request_metadata_sqlstate(code));
        }
        for code in ["08006", "40001", "40P01", "53300", "57P01"] {
            assert!(!is_permanent_request_metadata_sqlstate(code));
        }
        assert_eq!(checked_stream_lag(None).unwrap(), None);
    }
}
