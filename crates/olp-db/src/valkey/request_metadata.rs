use std::time::Duration;

use chrono::{DateTime, Utc};
use olp_engine::inference::request_metadata::Event;
use redis::{
    AsyncCommands, Value,
    aio::{ConnectionManager, ConnectionManagerConfig},
    streams::{StreamInfoGroupsReply, StreamPendingReply},
};
use tokio::sync::watch;
use tracing::{error, warn};

use crate::{
    error::Error as PersistenceError,
    request_metadata::{ingestion::Outcome, reconciliation::Gap},
    store::Store,
    worker_health::RequestMetadataConsumerActivity,
};

use super::Error;

mod protocol;

use protocol::{
    AutoClaimPage, StreamEntry, parse_auto_claim_reply, parse_stream_id, parse_xread_reply,
};

const AUTOCLAIM_SCRIPT: &str = include_str!("../../scripts/claim_request_metadata.lua");

pub const LEGACY_REQUEST_METADATA_STREAM: &str = "olp:v2:request-metadata";
const REQUEST_METADATA_GROUP: &str = "olp:persistence";
const REQUEST_METADATA_BATCH_SIZE: usize = 100;
const REQUEST_METADATA_BLOCK_INTERVAL: Duration = Duration::from_secs(1);
const REQUEST_METADATA_ACTIVE_RECOVERY_BLOCK_INTERVAL: Duration = Duration::from_millis(10);
const REQUEST_METADATA_OWN_PENDING_INTERVAL: Duration = Duration::from_secs(1);
const REQUEST_METADATA_HEALTH_INTERVAL: Duration = Duration::from_secs(5);
const REQUEST_METADATA_RETRY_MIN: Duration = Duration::from_millis(100);
const REQUEST_METADATA_RETRY_MAX: Duration = Duration::from_secs(5);
const REQUEST_METADATA_RESPONSE_TIMEOUT_MARGIN: Duration = Duration::from_secs(1);

/// A delivery must remain idle for this long before another process may steal
/// it. `XAUTOCLAIM` atomically transfers ownership and resets this idle clock.
pub const REQUEST_METADATA_RECLAIM_IDLE: Duration = Duration::from_secs(30);

/// Survivors start a stale-Pending-Entry-List scan at startup and at least
/// this often thereafter. Subject to a bounded backlog and service
/// availability, an abandoned entry is therefore reclaimable within the idle
/// threshold plus this interval.
pub const REQUEST_METADATA_RECOVERY_INTERVAL: Duration = Duration::from_secs(5);

#[derive(Clone, Copy)]
struct ConsumerPolicy {
    batch_size: usize,
    block_interval: Duration,
    active_recovery_block_interval: Duration,
    own_pending_interval: Duration,
    reclaim_idle: Duration,
    recovery_interval: Duration,
    health_interval: Duration,
}

impl Default for ConsumerPolicy {
    fn default() -> Self {
        Self {
            batch_size: REQUEST_METADATA_BATCH_SIZE,
            block_interval: REQUEST_METADATA_BLOCK_INTERVAL,
            active_recovery_block_interval: REQUEST_METADATA_ACTIVE_RECOVERY_BLOCK_INTERVAL,
            own_pending_interval: REQUEST_METADATA_OWN_PENDING_INTERVAL,
            reclaim_idle: REQUEST_METADATA_RECLAIM_IDLE,
            recovery_interval: REQUEST_METADATA_RECOVERY_INTERVAL,
            health_interval: REQUEST_METADATA_HEALTH_INTERVAL,
        }
    }
}

#[derive(Clone, Copy, Debug, Default)]
struct ProcessingSummary {
    retry: bool,
    shutdown: bool,
    completed: u64,
    duplicates: u64,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum EntryProcessingOutcome {
    Completed { duplicate: bool },
    Retry,
}

/// Runs one request-metadata consumer identity until shutdown. Callers must
/// keep `consumer` stable across supervisor reconnects and unique across live
/// worker processes. PostgreSQL is always committed before the atomic
/// `XACK`/`XDEL` transaction.
pub async fn run_request_metadata_consumer(
    store: &Store,
    valkey_url: &str,
    stream: &str,
    consumer: &str,
    shutdown: watch::Receiver<bool>,
) -> Result<(), Error> {
    run_request_metadata_consumer_with_policy(
        store,
        valkey_url,
        stream,
        consumer,
        shutdown,
        ConsumerPolicy::default(),
    )
    .await
}

async fn run_request_metadata_consumer_with_policy(
    store: &Store,
    valkey_url: &str,
    stream: &str,
    consumer: &str,
    mut shutdown: watch::Receiver<bool>,
    policy: ConsumerPolicy,
) -> Result<(), Error> {
    validate_configuration(stream, consumer, policy)?;
    let longest_block_interval = policy
        .block_interval
        .max(policy.active_recovery_block_interval);
    let mut connection = request_metadata_connection(valkey_url, longest_block_interval).await?;
    create_consumer_group(&mut connection, stream).await?;
    #[cfg(all(feature = "test-util", debug_assertions))]
    {
        let _: () = connection
            .client_setname(format!("olp-test-request-metadata-{consumer}"))
            .await?;
    }

    let started_at = tokio::time::Instant::now();
    let mut own_pending_start = "0-0".to_owned();
    let mut own_pending_due = started_at;
    let mut stale_start = "0-0".to_owned();
    let mut stale_due = started_at;
    let mut health_due = started_at;
    let mut retry_delay = REQUEST_METADATA_RETRY_MIN;

    loop {
        if *shutdown.borrow() {
            // Never delete this consumer on exit: a delivered batch may still
            // be uncommitted in its PEL and must remain reclaimable.
            return Ok(());
        }

        // One bounded own-Pending-Entry-List page and one bounded stale scan
        // precede every blocking new-entry read. Full recovery pages shorten
        // (but do not remove) that block, interleaving both sources so neither
        // can starve the other.
        let mut recovery_active = false;
        let mut cycle_retry = false;
        let now = tokio::time::Instant::now();

        if now >= own_pending_due {
            let entries = read_group(
                &mut connection,
                stream,
                consumer,
                &own_pending_start,
                policy.batch_size,
                None,
            )
            .await?;
            let full_batch = entries.len() == policy.batch_size;
            let next_start = entries
                .last()
                .map_or_else(|| "0-0".to_owned(), |entry| entry.id.clone());
            let summary =
                process_entries(store, &mut connection, stream, consumer, entries, &shutdown)
                    .await?;
            report_processing_activity(store, summary, true).await?;
            if summary.shutdown {
                return Ok(());
            }
            cycle_retry |= summary.retry;
            if full_batch {
                own_pending_start = next_start;
                own_pending_due = tokio::time::Instant::now();
                recovery_active = true;
            } else {
                own_pending_start = "0-0".to_owned();
                own_pending_due = tokio::time::Instant::now() + policy.own_pending_interval;
            }
        }

        if tokio::time::Instant::now() >= stale_due {
            let page = auto_claim(
                &mut connection,
                stream,
                consumer,
                policy.reclaim_idle,
                &stale_start,
                policy.batch_size,
            )
            .await?;
            // XAUTOCLAIM has already destroyed the evidence for these IDs:
            // the server dropped them from the group PEL and will not report
            // them again. Record the gaps before anything that can fail, and
            // never let a purely informational counter abort the batch first.
            report_deleted_pending_entries(store, consumer, &page.deleted_ids).await?;
            if !page.entries.is_empty() {
                let reclaimed = u64::try_from(page.entries.len())
                    .map_err(|_| Error::InvalidState("reclaimed entry count overflow"))?;
                if let Err(error) = store
                    .report_request_metadata_consumer_activity(RequestMetadataConsumerActivity {
                        reclaimed,
                        ..RequestMetadataConsumerActivity::default()
                    })
                    .await
                {
                    warn!(%error, "reclaimed request metadata counter was not recorded");
                }
            }
            let next_start = page.next_start;
            let summary = process_entries(
                store,
                &mut connection,
                stream,
                consumer,
                page.entries,
                &shutdown,
            )
            .await?;
            report_processing_activity(store, summary, true).await?;
            if summary.shutdown {
                return Ok(());
            }
            cycle_retry |= summary.retry;
            if next_start == "0-0" {
                stale_start = next_start;
                stale_due = tokio::time::Instant::now() + policy.recovery_interval;
            } else {
                stale_start = next_start;
                stale_due = tokio::time::Instant::now();
                recovery_active = true;
            }
        }

        if tokio::time::Instant::now() >= health_due {
            checkpoint_request_metadata_consumer_health(store, &mut connection, stream).await?;
            health_due = tokio::time::Instant::now() + policy.health_interval;
        }

        if cycle_retry && wait_for_retry(&mut shutdown, retry_delay).await {
            return Ok(());
        }

        if cycle_retry {
            retry_delay = (retry_delay * 2).min(REQUEST_METADATA_RETRY_MAX);
        } else {
            retry_delay = REQUEST_METADATA_RETRY_MIN;
        }

        let block_interval = if recovery_active {
            policy.active_recovery_block_interval
        } else {
            policy.block_interval
        };
        let entries = tokio::select! {
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return Ok(());
                }
                continue;
            }
            result = read_group(
                &mut connection,
                stream,
                consumer,
                ">",
                policy.batch_size,
                Some(block_interval),
            ) => result?,
        };
        let summary =
            process_entries(store, &mut connection, stream, consumer, entries, &shutdown).await?;
        report_processing_activity(store, summary, false).await?;
        if summary.shutdown {
            return Ok(());
        }
        if summary.retry {
            own_pending_start = "0-0".to_owned();
            own_pending_due = tokio::time::Instant::now() + retry_delay;
            if wait_for_retry(&mut shutdown, retry_delay).await {
                return Ok(());
            }
            retry_delay = (retry_delay * 2).min(REQUEST_METADATA_RETRY_MAX);
        }
    }
}

async fn request_metadata_connection(
    valkey_url: &str,
    block_interval: Duration,
) -> Result<ConnectionManager, Error> {
    let response_timeout = block_interval
        .checked_add(REQUEST_METADATA_RESPONSE_TIMEOUT_MARGIN)
        .ok_or(Error::InvalidState(
            "request metadata response timeout overflow",
        ))?;
    let client = redis::Client::open(valkey_url)?;
    let config = ConnectionManagerConfig::new().set_response_timeout(Some(response_timeout));
    Ok(ConnectionManager::new_with_config(client, config).await?)
}

fn validate_configuration(
    stream: &str,
    consumer: &str,
    policy: ConsumerPolicy,
) -> Result<(), Error> {
    if stream.is_empty() || consumer.is_empty() {
        return Err(Error::InvalidState("empty stream or consumer name"));
    }
    if policy.batch_size == 0 || policy.batch_size > 1_000 {
        return Err(Error::InvalidState("invalid request metadata batch size"));
    }
    for interval in [
        policy.block_interval,
        policy.active_recovery_block_interval,
        policy.own_pending_interval,
        policy.recovery_interval,
        policy.health_interval,
    ] {
        checked_milliseconds(interval)?;
    }
    checked_milliseconds_allow_zero(policy.reclaim_idle)?;
    Ok(())
}

async fn create_consumer_group(
    connection: &mut ConnectionManager,
    stream: &str,
) -> Result<(), Error> {
    let result: Result<String, redis::RedisError> = redis::cmd("XGROUP")
        .arg("CREATE")
        .arg(stream)
        .arg(REQUEST_METADATA_GROUP)
        .arg("0")
        .arg("MKSTREAM")
        .query_async(connection)
        .await;
    match result {
        Ok(reply) if reply == "OK" => Ok(()),
        Ok(_) => Err(Error::InvalidState("invalid consumer group creation reply")),
        Err(error) if error.code() == Some("BUSYGROUP") => Ok(()),
        Err(error) => Err(error.into()),
    }
}

async fn read_group(
    connection: &mut ConnectionManager,
    stream: &str,
    consumer: &str,
    id: &str,
    batch_size: usize,
    block: Option<Duration>,
) -> Result<Vec<StreamEntry>, Error> {
    let mut command = redis::cmd("XREADGROUP");
    command
        .arg("GROUP")
        .arg(REQUEST_METADATA_GROUP)
        .arg(consumer)
        .arg("COUNT")
        .arg(batch_size);
    if let Some(block) = block {
        command.arg("BLOCK").arg(checked_milliseconds(block)?);
    }
    let reply: Value = command
        .arg("STREAMS")
        .arg(stream)
        .arg(id)
        .query_async(connection)
        .await?;
    parse_xread_reply(reply, stream, batch_size)
}

async fn auto_claim(
    connection: &mut ConnectionManager,
    stream: &str,
    consumer: &str,
    min_idle: Duration,
    start: &str,
    batch_size: usize,
) -> Result<AutoClaimPage, Error> {
    let reply: Value = redis::Script::new(AUTOCLAIM_SCRIPT)
        .key(stream)
        .arg(REQUEST_METADATA_GROUP)
        .arg(consumer)
        .arg(checked_milliseconds_allow_zero(min_idle)?)
        .arg(start)
        .arg(batch_size)
        .invoke_async(connection)
        .await?;
    parse_auto_claim_reply(reply, batch_size)
}

async fn process_entries(
    store: &Store,
    connection: &mut ConnectionManager,
    stream: &str,
    consumer: &str,
    entries: Vec<StreamEntry>,
    shutdown: &watch::Receiver<bool>,
) -> Result<ProcessingSummary, Error> {
    let mut summary = ProcessingSummary::default();
    for entry in entries {
        if *shutdown.borrow() {
            summary.shutdown = true;
            break;
        }
        match process_entry(store, connection, stream, consumer, entry).await? {
            EntryProcessingOutcome::Completed { duplicate } => {
                summary.completed = summary.completed.saturating_add(1);
                summary.duplicates = summary.duplicates.saturating_add(u64::from(duplicate));
            }
            EntryProcessingOutcome::Retry => summary.retry = true,
        }
    }
    Ok(summary)
}

async fn report_processing_activity(
    store: &Store,
    summary: ProcessingSummary,
    recovered: bool,
) -> Result<(), Error> {
    if summary.completed == 0 && summary.duplicates == 0 {
        return Ok(());
    }
    store
        .report_request_metadata_consumer_activity(RequestMetadataConsumerActivity {
            recovered: if recovered { summary.completed } else { 0 },
            duplicates: summary.duplicates,
            processed: summary.completed,
            ..RequestMetadataConsumerActivity::default()
        })
        .await?;
    Ok(())
}

/// Returns `true` when PostgreSQL rejected the attempt transiently and the
/// entry must remain in the PEL for a later pass.
async fn process_entry(
    store: &Store,
    connection: &mut ConnectionManager,
    stream: &str,
    consumer: &str,
    entry: StreamEntry,
) -> Result<EntryProcessingOutcome, Error> {
    if entry.deleted_pending_id.is_some() {
        return finish_deleted_pending_marker(store, connection, stream, consumer, &entry).await;
    }
    let Some(payload) = entry.payload else {
        // The entry is still in this consumer's PEL but its payload is gone:
        // the stream entry was deleted. That is stream loss, not a producer
        // writing a bad event, and the XAUTOCLAIM path already files it as
        // such. Classify both the same way so operators look in one place.
        error!(stream_id = %entry.id, "request metadata stream event payload is missing");
        let now = Utc::now();
        let result = store
            .report_request_metadata_gap_once(
                Gap {
                    gateway_instance: consumer.to_owned(),
                    event_count: 1,
                    reason: "missing_stream_event".to_owned(),
                    first_observed_at: now,
                    last_observed_at: now,
                },
                &format!("request-metadata-stream:{}:missing", entry.id),
            )
            .await;
        return finish_gap_or_retry(result, connection, stream, &entry.id).await;
    };
    let event = match serde_json::from_slice::<Event>(&payload) {
        Ok(event) => event,
        Err(_) => {
            error!(stream_id = %entry.id, "discarding malformed request metadata stream event");
            let now = Utc::now();
            let result = store
                .report_request_metadata_gap_once(
                    Gap {
                        gateway_instance: consumer.to_owned(),
                        event_count: 1,
                        reason: "malformed_stream_event".to_owned(),
                        first_observed_at: now,
                        last_observed_at: now,
                    },
                    &format!("request-metadata-stream:{}:malformed", entry.id),
                )
                .await;
            return finish_gap_or_retry(result, connection, stream, &entry.id).await;
        }
    };

    #[cfg(all(feature = "test-util", debug_assertions))]
    if event.usage_complete {
        block_after_test_delivery().await;
    }

    match store
        .persist_request_metadata_stream_event(&event, &payload)
        .await
    {
        Ok(outcome) => {
            if outcome == Outcome::RejectedOutsideReplayWindow {
                warn!(stream_id = %entry.id, "request metadata event outside the replay window was recorded as an uncertain gap");
            }
            acknowledge_and_delete(connection, stream, &entry.id).await?;
            Ok(EntryProcessingOutcome::Completed {
                duplicate: outcome == Outcome::Duplicate,
            })
        }
        Err(PersistenceError::InvalidRequestMetadataEvent) => {
            error!(stream_id = %entry.id, "discarding permanently invalid request metadata event");
            let observed_at = Utc::now();
            let result = store
                .report_request_metadata_gap_once(
                    Gap {
                        gateway_instance: consumer.to_owned(),
                        event_count: 1,
                        reason: "invalid_request_metadata_event".to_owned(),
                        first_observed_at: observed_at,
                        last_observed_at: observed_at,
                    },
                    &format!("request-metadata-event:{}:invalid", event.event_id),
                )
                .await;
            finish_gap_or_retry(result, connection, stream, &entry.id).await
        }
        Err(error) => {
            warn!(%error, stream_id = %entry.id, "request metadata persistence will retry");
            Ok(EntryProcessingOutcome::Retry)
        }
    }
}

async fn finish_deleted_pending_marker(
    store: &Store,
    connection: &mut ConnectionManager,
    stream: &str,
    consumer: &str,
    entry: &StreamEntry,
) -> Result<EntryProcessingOutcome, Error> {
    let deleted_id = entry
        .deleted_pending_id
        .as_deref()
        .ok_or(Error::InvalidState("deleted pending marker is empty"))?;
    let now = Utc::now();
    let result = store
        .report_request_metadata_gap_once(
            Gap {
                gateway_instance: consumer.to_owned(),
                event_count: 1,
                reason: "missing_stream_event".to_owned(),
                first_observed_at: now,
                last_observed_at: now,
            },
            &format!("request-metadata-stream:{deleted_id}:missing"),
        )
        .await;
    finish_gap_or_retry(result, connection, stream, &entry.id).await
}

#[cfg(all(feature = "test-util", debug_assertions))]
async fn block_after_test_delivery() {
    let Ok(marker) = std::env::var("OLP_TEST_REQUEST_METADATA_OWNED_MARKER") else {
        return;
    };
    let marker = std::path::Path::new(&marker);
    match std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(marker)
    {
        Ok(_) => std::future::pending::<()>().await,
        Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => {}
        Err(error) => panic!("failed to create request-metadata failpoint marker: {error}"),
    }
}

async fn finish_gap_or_retry(
    result: Result<bool, PersistenceError>,
    connection: &mut ConnectionManager,
    stream: &str,
    id: &str,
) -> Result<EntryProcessingOutcome, Error> {
    match result {
        Ok(_) => {
            acknowledge_and_delete(connection, stream, id).await?;
            Ok(EntryProcessingOutcome::Completed { duplicate: false })
        }
        Err(error) => {
            warn!(%error, stream_id = %id, "request metadata gap persistence will retry");
            Ok(EntryProcessingOutcome::Retry)
        }
    }
}

async fn acknowledge_and_delete(
    connection: &mut ConnectionManager,
    stream: &str,
    id: &str,
) -> Result<(), Error> {
    let (acknowledged, deleted): (usize, usize) = redis::pipe()
        .atomic()
        .cmd("XACK")
        .arg(stream)
        .arg(REQUEST_METADATA_GROUP)
        .arg(id)
        .cmd("XDEL")
        .arg(stream)
        .arg(id)
        .query_async(connection)
        .await?;
    if acknowledged > 1 || deleted > 1 {
        return Err(Error::InvalidState("invalid stream acknowledgement reply"));
    }
    Ok(())
}

async fn report_deleted_pending_entries(
    store: &Store,
    consumer: &str,
    deleted_ids: &[String],
) -> Result<(), Error> {
    for id in deleted_ids {
        let now = Utc::now();
        store
            .report_request_metadata_gap_once(
                Gap {
                    gateway_instance: consumer.to_owned(),
                    event_count: 1,
                    reason: "missing_stream_event".to_owned(),
                    first_observed_at: now,
                    last_observed_at: now,
                },
                &format!("request-metadata-stream:{id}:missing"),
            )
            .await?;
    }
    Ok(())
}

async fn checkpoint_request_metadata_consumer_health(
    store: &Store,
    connection: &mut ConnectionManager,
    stream: &str,
) -> Result<(), Error> {
    let pending: StreamPendingReply = connection.xpending(stream, REQUEST_METADATA_GROUP).await?;
    let (pending_events, oldest_pending_at) = match pending {
        StreamPendingReply::Empty => (0_u64, None),
        StreamPendingReply::Data(data) => {
            let (millis, _) = parse_stream_id(&data.start_id)?;
            let millis = i64::try_from(millis)
                .map_err(|_| Error::InvalidState("pending stream ID overflow"))?;
            let timestamp = DateTime::<Utc>::from_timestamp_millis(millis)
                .ok_or(Error::InvalidState("invalid pending stream ID timestamp"))?;
            let count = u64::try_from(data.count)
                .map_err(|_| Error::InvalidState("pending count overflow"))?;
            (count, Some(timestamp))
        }
        _ => {
            return Err(Error::InvalidState("unrecognized pending stream reply"));
        }
    };
    let groups: StreamInfoGroupsReply = connection.xinfo_groups(stream).await?;
    let group = groups
        .groups
        .into_iter()
        .find(|candidate| candidate.name == REQUEST_METADATA_GROUP)
        .ok_or(Error::InvalidState("consumer group disappeared"))?;
    // Valkey may transiently return a null lag while concurrent deliveries
    // and deletions advance the group. This stream deletes every acknowledged
    // entry in the same transaction as XACK, so its remaining length is the
    // sum of pending payloads and not-yet-delivered entries. Use that
    // conservative group-wide fallback instead of either killing the worker
    // or falsely coercing an unknown lag to zero.
    let lag = match group.lag {
        Some(lag) => lag,
        None => {
            let stream_length: usize = connection.xlen(stream).await?;
            stream_length.saturating_sub(group.pending)
        }
    };
    let lag_events = u64::try_from(lag).map_err(|_| Error::InvalidState("stream lag overflow"))?;
    store
        .report_request_metadata_consumer_health(pending_events, lag_events, oldest_pending_at)
        .await?;
    Ok(())
}

async fn wait_for_retry(shutdown: &mut watch::Receiver<bool>, delay: Duration) -> bool {
    tokio::select! {
        changed = shutdown.changed() => changed.is_err() || *shutdown.borrow(),
        () = tokio::time::sleep(delay) => false,
    }
}

fn checked_milliseconds(duration: Duration) -> Result<u64, Error> {
    if duration.is_zero() {
        return Err(Error::InvalidState(
            "request metadata interval must be positive",
        ));
    }
    checked_milliseconds_allow_zero(duration)
}

fn checked_milliseconds_allow_zero(duration: Duration) -> Result<u64, Error> {
    u64::try_from(duration.as_millis())
        .map_err(|_| Error::InvalidState("request metadata interval millisecond overflow"))
}

#[cfg(all(feature = "test-util", debug_assertions))]
pub mod test_support {
    use super::*;

    #[derive(Clone, Copy, Debug)]
    pub struct RequestMetadataConsumerTestPolicy {
        pub batch_size: usize,
        pub block_interval: Duration,
        pub active_recovery_block_interval: Duration,
        pub own_pending_interval: Duration,
        pub reclaim_idle: Duration,
        pub recovery_interval: Duration,
        pub health_interval: Duration,
    }

    impl Default for RequestMetadataConsumerTestPolicy {
        fn default() -> Self {
            let short = Duration::from_millis(10);
            Self {
                batch_size: 10,
                block_interval: short,
                active_recovery_block_interval: Duration::from_millis(1),
                own_pending_interval: short,
                reclaim_idle: Duration::ZERO,
                recovery_interval: short,
                health_interval: short,
            }
        }
    }

    pub async fn run_request_metadata_consumer(
        store: &Store,
        valkey_url: &str,
        stream: &str,
        consumer: &str,
        shutdown: watch::Receiver<bool>,
        policy: RequestMetadataConsumerTestPolicy,
    ) -> Result<(), Error> {
        run_request_metadata_consumer_with_policy(
            store,
            valkey_url,
            stream,
            consumer,
            shutdown,
            ConsumerPolicy {
                batch_size: policy.batch_size,
                block_interval: policy.block_interval,
                active_recovery_block_interval: policy.active_recovery_block_interval,
                own_pending_interval: policy.own_pending_interval,
                reclaim_idle: policy.reclaim_idle,
                recovery_interval: policy.recovery_interval,
                health_interval: policy.health_interval,
            },
        )
        .await
    }
}
