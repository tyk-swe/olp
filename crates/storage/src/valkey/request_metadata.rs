use std::{collections::HashSet, time::Duration};

use chrono::{DateTime, Utc};
use redis::{
    AsyncCommands, Value,
    aio::ConnectionManager,
    streams::{StreamInfoGroupsReply, StreamPendingReply},
};
use tokio::sync::watch;
use tracing::{error, warn};

use crate::{
    PersistenceError, PgStore,
    request_metadata::{
        RequestMetadataEvent, RequestMetadataGap, RequestMetadataPersistenceOutcome,
    },
    worker_health::RequestMetadataConsumerActivity,
};

use super::{ValkeyAdapterError, valkey_connection};

pub const REQUEST_METADATA_STREAM: &str = "olp:v2:request-metadata";
const REQUEST_METADATA_GROUP: &str = "olp:persistence";
const REQUEST_METADATA_BATCH_SIZE: usize = 100;
const REQUEST_METADATA_BLOCK_INTERVAL: Duration = Duration::from_secs(1);
const REQUEST_METADATA_ACTIVE_RECOVERY_BLOCK_INTERVAL: Duration = Duration::from_millis(10);
const REQUEST_METADATA_OWN_PENDING_INTERVAL: Duration = Duration::from_secs(1);
const REQUEST_METADATA_HEALTH_INTERVAL: Duration = Duration::from_secs(5);
const REQUEST_METADATA_RETRY_MIN: Duration = Duration::from_millis(100);
const REQUEST_METADATA_RETRY_MAX: Duration = Duration::from_secs(5);

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

#[derive(Debug)]
struct StreamEntry {
    id: String,
    payload: Option<Vec<u8>>,
}

#[derive(Debug)]
struct AutoClaimPage {
    next_start: String,
    entries: Vec<StreamEntry>,
    deleted_ids: Vec<String>,
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
    store: &PgStore,
    valkey_url: &str,
    stream: &str,
    consumer: &str,
    shutdown: watch::Receiver<bool>,
) -> Result<(), ValkeyAdapterError> {
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
    store: &PgStore,
    valkey_url: &str,
    stream: &str,
    consumer: &str,
    mut shutdown: watch::Receiver<bool>,
    policy: ConsumerPolicy,
) -> Result<(), ValkeyAdapterError> {
    validate_configuration(stream, consumer, policy)?;
    let mut connection = valkey_connection(valkey_url).await?;
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
            store
                .report_request_metadata_consumer_activity(RequestMetadataConsumerActivity {
                    reclaimed: u64::try_from(page.entries.len()).map_err(|_| {
                        ValkeyAdapterError::InvalidState("reclaimed entry count overflow")
                    })?,
                    ..RequestMetadataConsumerActivity::default()
                })
                .await?;
            report_deleted_pending_entries(store, consumer, &page.deleted_ids).await?;
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

fn validate_configuration(
    stream: &str,
    consumer: &str,
    policy: ConsumerPolicy,
) -> Result<(), ValkeyAdapterError> {
    if stream.is_empty() || consumer.is_empty() {
        return Err(ValkeyAdapterError::InvalidState(
            "empty stream or consumer name",
        ));
    }
    if policy.batch_size == 0 || policy.batch_size > 1_000 {
        return Err(ValkeyAdapterError::InvalidState(
            "invalid request metadata batch size",
        ));
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
) -> Result<(), ValkeyAdapterError> {
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
        Ok(_) => Err(ValkeyAdapterError::InvalidState(
            "invalid consumer group creation reply",
        )),
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
) -> Result<Vec<StreamEntry>, ValkeyAdapterError> {
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
) -> Result<AutoClaimPage, ValkeyAdapterError> {
    let reply: Value = redis::cmd("XAUTOCLAIM")
        .arg(stream)
        .arg(REQUEST_METADATA_GROUP)
        .arg(consumer)
        .arg(checked_milliseconds_allow_zero(min_idle)?)
        .arg(start)
        .arg("COUNT")
        .arg(batch_size)
        .query_async(connection)
        .await?;
    parse_auto_claim_reply(reply, batch_size)
}

async fn process_entries(
    store: &PgStore,
    connection: &mut ConnectionManager,
    stream: &str,
    consumer: &str,
    entries: Vec<StreamEntry>,
    shutdown: &watch::Receiver<bool>,
) -> Result<ProcessingSummary, ValkeyAdapterError> {
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
    store: &PgStore,
    summary: ProcessingSummary,
    recovered: bool,
) -> Result<(), ValkeyAdapterError> {
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
    store: &PgStore,
    connection: &mut ConnectionManager,
    stream: &str,
    consumer: &str,
    entry: StreamEntry,
) -> Result<EntryProcessingOutcome, ValkeyAdapterError> {
    let Some(payload) = entry.payload else {
        error!(stream_id = %entry.id, "discarding malformed request metadata stream event");
        let now = Utc::now();
        let result = store
            .report_request_metadata_gap_once(
                RequestMetadataGap {
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
    };
    let event = match serde_json::from_slice::<RequestMetadataEvent>(&payload) {
        Ok(event) => event,
        Err(_) => {
            error!(stream_id = %entry.id, "discarding malformed request metadata stream event");
            let now = Utc::now();
            let result = store
                .report_request_metadata_gap_once(
                    RequestMetadataGap {
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

    match store
        .persist_request_metadata_stream_event(&event, &payload)
        .await
    {
        Ok(outcome) => {
            if outcome == RequestMetadataPersistenceOutcome::RejectedOutsideReplayWindow {
                warn!(stream_id = %entry.id, "request metadata event outside the replay window was recorded as an uncertain gap");
            }
            acknowledge_and_delete(connection, stream, &entry.id).await?;
            Ok(EntryProcessingOutcome::Completed {
                duplicate: outcome == RequestMetadataPersistenceOutcome::Duplicate,
            })
        }
        Err(PersistenceError::InvalidRequestMetadataEvent) => {
            error!(stream_id = %entry.id, "discarding permanently invalid request metadata event");
            let observed_at = Utc::now();
            let result = store
                .report_request_metadata_gap_once(
                    RequestMetadataGap {
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

async fn finish_gap_or_retry(
    result: Result<bool, PersistenceError>,
    connection: &mut ConnectionManager,
    stream: &str,
    id: &str,
) -> Result<EntryProcessingOutcome, ValkeyAdapterError> {
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
) -> Result<(), ValkeyAdapterError> {
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
        return Err(ValkeyAdapterError::InvalidState(
            "invalid stream acknowledgement reply",
        ));
    }
    Ok(())
}

async fn report_deleted_pending_entries(
    store: &PgStore,
    consumer: &str,
    deleted_ids: &[String],
) -> Result<(), ValkeyAdapterError> {
    for id in deleted_ids {
        let now = Utc::now();
        store
            .report_request_metadata_gap_once(
                RequestMetadataGap {
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
    store: &PgStore,
    connection: &mut ConnectionManager,
    stream: &str,
) -> Result<(), ValkeyAdapterError> {
    let pending: StreamPendingReply = connection.xpending(stream, REQUEST_METADATA_GROUP).await?;
    let (pending_events, oldest_pending_at) = match pending {
        StreamPendingReply::Empty => (0_u64, None),
        StreamPendingReply::Data(data) => {
            let (millis, _) = parse_stream_id(&data.start_id)?;
            let millis = i64::try_from(millis)
                .map_err(|_| ValkeyAdapterError::InvalidState("pending stream ID overflow"))?;
            let timestamp = DateTime::<Utc>::from_timestamp_millis(millis).ok_or(
                ValkeyAdapterError::InvalidState("invalid pending stream ID timestamp"),
            )?;
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
    let groups: StreamInfoGroupsReply = connection.xinfo_groups(stream).await?;
    let group = groups
        .groups
        .into_iter()
        .find(|candidate| candidate.name == REQUEST_METADATA_GROUP)
        .ok_or(ValkeyAdapterError::InvalidState(
            "consumer group disappeared",
        ))?;
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
    let lag_events =
        u64::try_from(lag).map_err(|_| ValkeyAdapterError::InvalidState("stream lag overflow"))?;
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

fn parse_xread_reply(
    reply: Value,
    expected_stream: &str,
    batch_size: usize,
) -> Result<Vec<StreamEntry>, ValkeyAdapterError> {
    let (stream, entries) = match reply {
        Value::Nil => return Ok(Vec::new()),
        Value::Array(mut streams) if streams.len() == 1 => {
            let stream = streams.pop().expect("one stream reply was validated");
            let Value::Array(mut pair) = stream else {
                return Err(ValkeyAdapterError::InvalidState(
                    "invalid XREADGROUP stream tuple",
                ));
            };
            if pair.len() != 2 {
                return Err(ValkeyAdapterError::InvalidState(
                    "invalid XREADGROUP stream tuple length",
                ));
            }
            let entries = pair.pop().expect("stream tuple length was validated");
            let stream = pair.pop().expect("stream tuple length was validated");
            (stream, entries)
        }
        Value::Map(mut streams) if streams.len() == 1 => {
            streams.pop().expect("one stream map entry was validated")
        }
        _ => {
            return Err(ValkeyAdapterError::InvalidState("invalid XREADGROUP reply"));
        }
    };
    if value_bytes(stream).as_deref() != Some(expected_stream.as_bytes()) {
        return Err(ValkeyAdapterError::InvalidState(
            "XREADGROUP returned an unexpected stream",
        ));
    }
    parse_entries(entries, batch_size)
}

fn parse_auto_claim_reply(
    reply: Value,
    batch_size: usize,
) -> Result<AutoClaimPage, ValkeyAdapterError> {
    let Value::Array(mut items) = reply else {
        return Err(ValkeyAdapterError::InvalidState("invalid XAUTOCLAIM reply"));
    };
    if !(2..=3).contains(&items.len()) {
        return Err(ValkeyAdapterError::InvalidState(
            "invalid XAUTOCLAIM reply length",
        ));
    }
    let deleted = if items.len() == 3 {
        items.pop().expect("XAUTOCLAIM reply length was validated")
    } else {
        Value::Array(Vec::new())
    };
    let entries = items.pop().expect("XAUTOCLAIM reply length was validated");
    let next_start = value_string(items.pop().expect("XAUTOCLAIM reply length was validated"))?;
    parse_stream_id(&next_start)?;
    let entries = parse_entries(entries, batch_size)?;
    let deleted_ids = parse_id_list(deleted)?;
    if deleted_ids.len() > batch_size.saturating_mul(10) {
        return Err(ValkeyAdapterError::InvalidState(
            "XAUTOCLAIM deleted-ID scan exceeded its protocol bound",
        ));
    }
    let claimed_ids = entries
        .iter()
        .map(|entry| entry.id.as_str())
        .collect::<HashSet<_>>();
    if deleted_ids
        .iter()
        .any(|id| claimed_ids.contains(id.as_str()))
    {
        return Err(ValkeyAdapterError::InvalidState(
            "XAUTOCLAIM returned overlapping claimed and deleted IDs",
        ));
    }
    Ok(AutoClaimPage {
        next_start,
        entries,
        deleted_ids,
    })
}

fn parse_entries(
    entries: Value,
    batch_size: usize,
) -> Result<Vec<StreamEntry>, ValkeyAdapterError> {
    let Value::Array(entries) = entries else {
        return Err(ValkeyAdapterError::InvalidState(
            "invalid stream entry list",
        ));
    };
    if entries.len() > batch_size {
        return Err(ValkeyAdapterError::InvalidState(
            "stream reply exceeded the requested batch size",
        ));
    }
    let mut ids = HashSet::with_capacity(entries.len());
    entries
        .into_iter()
        .map(|entry| {
            let Value::Array(mut fields) = entry else {
                return Err(ValkeyAdapterError::InvalidState(
                    "invalid stream entry tuple",
                ));
            };
            if fields.len() != 2 {
                return Err(ValkeyAdapterError::InvalidState(
                    "invalid stream entry tuple length",
                ));
            }
            let field_values = fields.pop().expect("entry tuple length was validated");
            let id = value_string(fields.pop().expect("entry tuple length was validated"))?;
            parse_stream_id(&id)?;
            if !ids.insert(id.clone()) {
                return Err(ValkeyAdapterError::InvalidState(
                    "stream reply contained a duplicate entry ID",
                ));
            }
            Ok(StreamEntry {
                id,
                payload: parse_event_payload(field_values)?,
            })
        })
        .collect()
}

fn parse_event_payload(fields: Value) -> Result<Option<Vec<u8>>, ValkeyAdapterError> {
    let pairs = match fields {
        Value::Nil => return Ok(None),
        Value::Array(values) => {
            if values.len() % 2 != 0 {
                return Err(ValkeyAdapterError::InvalidState(
                    "stream field list has odd length",
                ));
            }
            let mut values = values.into_iter();
            let mut pairs = Vec::with_capacity(values.len() / 2);
            while let Some(field) = values.next() {
                let value = values.next().expect("field list length was validated");
                pairs.push((field, value));
            }
            pairs
        }
        Value::Map(pairs) => pairs,
        _ => {
            return Err(ValkeyAdapterError::InvalidState(
                "invalid stream field container",
            ));
        }
    };
    if pairs.len() != 1 {
        return Ok(None);
    }
    let (field, value) = pairs
        .into_iter()
        .next()
        .expect("one stream field was validated");
    if value_bytes(field).as_deref() != Some(b"event") {
        return Ok(None);
    }
    Ok(value_bytes(value))
}

fn parse_id_list(value: Value) -> Result<Vec<String>, ValkeyAdapterError> {
    let Value::Array(values) = value else {
        return Err(ValkeyAdapterError::InvalidState(
            "invalid XAUTOCLAIM deleted-ID list",
        ));
    };
    let mut ids = HashSet::with_capacity(values.len());
    values
        .into_iter()
        .map(|value| {
            let id = value_string(value)?;
            parse_stream_id(&id)?;
            if !ids.insert(id.clone()) {
                return Err(ValkeyAdapterError::InvalidState(
                    "XAUTOCLAIM returned a duplicate deleted ID",
                ));
            }
            Ok(id)
        })
        .collect()
}

fn value_string(value: Value) -> Result<String, ValkeyAdapterError> {
    let bytes = value_bytes(value).ok_or(ValkeyAdapterError::InvalidState(
        "stream reply contained a non-string value",
    ))?;
    String::from_utf8(bytes)
        .map_err(|_| ValkeyAdapterError::InvalidState("stream reply contained invalid UTF-8"))
}

fn value_bytes(value: Value) -> Option<Vec<u8>> {
    match value {
        Value::BulkString(value) => Some(value),
        Value::SimpleString(value) => Some(value.into_bytes()),
        _ => None,
    }
}

fn parse_stream_id(id: &str) -> Result<(u64, u64), ValkeyAdapterError> {
    let (milliseconds, sequence) = id.split_once('-').ok_or(ValkeyAdapterError::InvalidState(
        "stream reply contained an invalid ID",
    ))?;
    if milliseconds.is_empty()
        || sequence.is_empty()
        || !milliseconds.bytes().all(|byte| byte.is_ascii_digit())
        || !sequence.bytes().all(|byte| byte.is_ascii_digit())
    {
        return Err(ValkeyAdapterError::InvalidState(
            "stream reply contained an invalid ID",
        ));
    }
    Ok((
        milliseconds.parse().map_err(|_| {
            ValkeyAdapterError::InvalidState("stream reply contained an overflowing ID")
        })?,
        sequence.parse().map_err(|_| {
            ValkeyAdapterError::InvalidState("stream reply contained an overflowing ID")
        })?,
    ))
}

fn checked_milliseconds(duration: Duration) -> Result<u64, ValkeyAdapterError> {
    if duration.is_zero() {
        return Err(ValkeyAdapterError::InvalidState(
            "request metadata interval must be positive",
        ));
    }
    checked_milliseconds_allow_zero(duration)
}

fn checked_milliseconds_allow_zero(duration: Duration) -> Result<u64, ValkeyAdapterError> {
    u64::try_from(duration.as_millis()).map_err(|_| {
        ValkeyAdapterError::InvalidState("request metadata interval millisecond overflow")
    })
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
        store: &PgStore,
        valkey_url: &str,
        stream: &str,
        consumer: &str,
        shutdown: watch::Receiver<bool>,
        policy: RequestMetadataConsumerTestPolicy,
    ) -> Result<(), ValkeyAdapterError> {
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

#[cfg(test)]
mod tests;
