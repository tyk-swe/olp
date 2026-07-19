//! Typed Valkey adapters for runtime hints and durable usage delivery.
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

use crate::{PersistenceError, PgStore, UsageEvent, UsageGap, UsagePersistenceOutcome};

const RUNTIME_HINT_CHANNEL: &str = "olp:v2:runtime";
const USAGE_STREAM: &str = "olp:v2:usage";
const USAGE_GROUP: &str = "olp:persistence";
const USAGE_CONSUMER: &str = "worker";

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

/// Runs the stable single-consumer usage worker until shutdown. PostgreSQL is
/// committed before an entry is acknowledged and deleted from Valkey.
pub async fn run_usage_consumer(
    store: &PgStore,
    valkey_url: &str,
    mut shutdown: watch::Receiver<bool>,
) -> Result<(), ValkeyAdapterError> {
    let mut connection = valkey_connection(valkey_url).await?;
    let create: Result<String, redis::RedisError> = redis::cmd("XGROUP")
        .arg("CREATE")
        .arg(USAGE_STREAM)
        .arg(USAGE_GROUP)
        .arg("0")
        .arg("MKSTREAM")
        .query_async(&mut connection)
        .await;
    if let Err(error) = create
        && !error.to_string().contains("BUSYGROUP")
    {
        return Err(error.into());
    }

    let mut drain_pending = true;
    let mut last_health_checkpoint =
        tokio::time::Instant::now() - std::time::Duration::from_secs(5);
    loop {
        let id = if drain_pending { "0" } else { ">" };
        let options = StreamReadOptions::default()
            .group(USAGE_GROUP, USAGE_CONSUMER)
            .count(100)
            .block(if drain_pending { 1 } else { 1_000 });
        let streams = [USAGE_STREAM];
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
                checkpoint_usage_consumer_health(store, &mut connection).await?;
                last_health_checkpoint = tokio::time::Instant::now();
            }
            continue;
        }

        for entry in entries {
            let payload = entry
                .map
                .get("event")
                .and_then(|value| redis::from_redis_value::<String>(value).ok());
            let event = payload
                .as_deref()
                .and_then(|payload| serde_json::from_str::<UsageEvent>(payload).ok());
            let Some(event) = event else {
                error!(stream_id = %entry.id, "discarding malformed usage stream event");
                let now = Utc::now();
                store
                    .report_usage_gap_once(
                        UsageGap {
                            gateway_instance: "stream-consumer".to_owned(),
                            event_count: 1,
                            reason: "malformed_stream_event".to_owned(),
                            first_observed_at: now,
                            last_observed_at: now,
                        },
                        &format!("usage-stream:{}:malformed", entry.id),
                    )
                    .await?;
                acknowledge_and_delete(&mut connection, &entry.id).await?;
                continue;
            };

            match store
                .persist_usage_stream_event(
                    &event,
                    payload
                        .as_deref()
                        .expect("a decoded usage event has its original payload")
                        .as_bytes(),
                )
                .await
            {
                Ok(outcome) => {
                    if outcome == UsagePersistenceOutcome::RejectedOutsideReplayWindow {
                        warn!(stream_id = %entry.id, "usage event outside the replay window was recorded as an uncertain gap");
                    }
                    acknowledge_and_delete(&mut connection, &entry.id).await?;
                }
                Err(PersistenceError::InvalidUsageEvent) => {
                    error!(stream_id = %entry.id, "discarding permanently invalid usage event");
                    let observed_at = Utc::now();
                    store
                        .report_usage_gap_once(
                            UsageGap {
                                gateway_instance: "stream-consumer".to_owned(),
                                event_count: 1,
                                reason: "invalid_usage_event".to_owned(),
                                first_observed_at: observed_at,
                                last_observed_at: observed_at,
                            },
                            &format!("usage-event:{}:invalid", event.event_id),
                        )
                        .await?;
                    acknowledge_and_delete(&mut connection, &entry.id).await?;
                }
                Err(error) => {
                    warn!(%error, stream_id = %entry.id, "usage persistence will retry");
                    drain_pending = true;
                    tokio::time::sleep(std::time::Duration::from_millis(100)).await;
                    break;
                }
            }
        }
        if last_health_checkpoint.elapsed() >= std::time::Duration::from_secs(5) {
            checkpoint_usage_consumer_health(store, &mut connection).await?;
            last_health_checkpoint = tokio::time::Instant::now();
        }
    }
}

async fn acknowledge_and_delete(
    connection: &mut ConnectionManager,
    id: &str,
) -> Result<(), ValkeyAdapterError> {
    let ids = [id];
    let _: usize = connection.xack(USAGE_STREAM, USAGE_GROUP, &ids).await?;
    let _: usize = connection.xdel(USAGE_STREAM, &ids).await?;
    Ok(())
}

async fn checkpoint_usage_consumer_health(
    store: &PgStore,
    connection: &mut ConnectionManager,
) -> Result<(), ValkeyAdapterError> {
    let pending: StreamPendingReply = connection.xpending(USAGE_STREAM, USAGE_GROUP).await?;
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
    };
    let groups: StreamInfoGroupsReply = connection.xinfo_groups(USAGE_STREAM).await?;
    let group = groups
        .groups
        .into_iter()
        .find(|candidate| candidate.name == USAGE_GROUP)
        .ok_or(ValkeyAdapterError::InvalidState(
            "consumer group disappeared",
        ))?;
    let lag_events = u64::try_from(group.lag.unwrap_or(0))
        .map_err(|_| ValkeyAdapterError::InvalidState("stream lag overflow"))?;
    store
        .report_usage_consumer_health(pending_events, lag_events, oldest_pending_at)
        .await?;
    Ok(())
}

async fn valkey_connection(url: &str) -> Result<ConnectionManager, redis::RedisError> {
    let client = redis::Client::open(url)?;
    ConnectionManager::new(client).await
}
