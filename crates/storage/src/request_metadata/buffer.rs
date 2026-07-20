use std::{sync::Arc, time::Duration};

use chrono::{DateTime, Utc};
use redis::{Client, RedisError, aio::ConnectionManager};
use thiserror::Error;
use tokio::sync::{mpsc, watch};
use uuid::Uuid;

use super::ingestion::RequestMetadataEvent;

const STREAM_WRITE_TIMEOUT: Duration = Duration::from_secs(1);

#[derive(Clone)]
pub struct RequestMetadataEmitter {
    pub(super) sender: mpsc::Sender<RequestMetadataEvent>,
    pub(super) health: Arc<RequestMetadataBufferHealth>,
}

impl RequestMetadataEmitter {
    pub fn bounded(capacity: usize) -> (Self, RequestMetadataReceiver) {
        let (sender, receiver) = mpsc::channel(capacity.max(1));
        let health = Arc::new(RequestMetadataBufferHealth::default());
        (
            Self {
                sender,
                health: Arc::clone(&health),
            },
            RequestMetadataReceiver { receiver, health },
        )
    }

    /// Never blocks the inference response path. Overflow is counted and made
    /// visible; callers must include this counter in readiness and metrics.
    pub fn emit(&self, event: RequestMetadataEvent) -> Result<(), RequestMetadataEmitError> {
        match self.sender.try_reserve() {
            Ok(permit) => {
                // Account for the event before publishing it. Receiver
                // shutdown drains until every outstanding permit is released,
                // so this reservation cannot disappear between close and len.
                self.health
                    .accepted
                    .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                permit.send(event);
                Ok(())
            }
            Err(mpsc::error::TrySendError::Full(_)) => {
                self.health.record_dropped(1);
                Err(RequestMetadataEmitError::Full)
            }
            Err(mpsc::error::TrySendError::Closed(_)) => {
                self.health.record_dropped(1);
                Err(RequestMetadataEmitError::Closed)
            }
        }
    }

    pub fn snapshot(&self) -> RequestMetadataBufferSnapshot {
        let mut snapshot = self.health.snapshot();
        snapshot.closed = self.sender.is_closed();
        snapshot
    }
}

#[derive(Debug, Clone, Copy, Error, PartialEq, Eq)]
pub enum RequestMetadataEmitError {
    #[error("the bounded request metadata buffer is full")]
    Full,
    #[error("the request metadata persistence worker is not running")]
    Closed,
}

pub struct RequestMetadataReceiver {
    receiver: mpsc::Receiver<RequestMetadataEvent>,
    health: Arc<RequestMetadataBufferHealth>,
}

impl RequestMetadataReceiver {
    /// Receives one buffered event. This supports alternate persistence
    /// adapters and deterministic integration tests without exposing prompts
    /// or response bodies (which are absent from [`RequestMetadataEvent`]).
    pub async fn recv_next(&mut self) -> Option<RequestMetadataEvent> {
        self.receiver.recv().await
    }

    /// Establishes the initial Valkey connection without dropping the bounded
    /// local queue when Valkey starts after the gateway. Once connected, the
    /// connection manager and [`Self::run`] handle subsequent reconnects.
    pub async fn run_connecting(
        mut self,
        valkey_url: &str,
        stream: &str,
        mut shutdown: watch::Receiver<bool>,
    ) -> Result<(), crate::ValkeyAdapterError> {
        let client = match Client::open(valkey_url) {
            Ok(client) => client,
            Err(error) => {
                self.record_abandoned(0).await;
                return Err(error.into());
            }
        };
        let mut backoff = Duration::from_millis(100);
        self.health
            .retrying
            .store(true, std::sync::atomic::Ordering::Relaxed);
        loop {
            let connection = tokio::select! {
                biased;
                changed = shutdown.changed() => {
                    if changed.is_err() || *shutdown.borrow() {
                        self.record_abandoned(0).await;
                        return Ok(());
                    }
                    continue;
                }
                connection = ConnectionManager::new(client.clone()) => connection,
            };
            if let Ok(mut connection) = connection {
                match crate::valkey::preflight_request_metadata_stream_connection(&mut connection)
                    .await
                {
                    Ok(()) => {
                        self.health
                            .retrying
                            .store(false, std::sync::atomic::Ordering::Relaxed);
                        return self
                            .run(connection, stream, shutdown)
                            .await
                            .map_err(Into::into);
                    }
                    Err(
                        error @ (crate::ValkeyAdapterError::LegacyRequestMetadataStreamNotDrained {
                            ..
                        }
                        | crate::ValkeyAdapterError::LegacyRequestMetadataStreamAcknowledgedEntries {
                            ..
                        }),
                    ) => {
                        self.record_abandoned(0).await;
                        return Err(error);
                    }
                    Err(_) => {}
                }
            }
            tokio::select! {
                biased;
                changed = shutdown.changed() => {
                    if changed.is_err() || *shutdown.borrow() {
                        self.record_abandoned(0).await;
                        return Ok(());
                    }
                        }
                () = tokio::time::sleep(backoff) => {}
            }
            backoff = (backoff * 2).min(Duration::from_secs(5));
        }
    }

    /// Writes to a Valkey Stream with bounded local buffering. On an outage the
    /// current event is retried, the channel fills to its configured bound, and
    /// further loss is explicitly counted by `RequestMetadataEmitter`.
    pub async fn run(
        mut self,
        mut connection: ConnectionManager,
        stream: &str,
        mut shutdown: watch::Receiver<bool>,
    ) -> Result<(), RedisError> {
        let mut shutdown_open = true;
        loop {
            if *shutdown.borrow() {
                self.record_abandoned(0).await;
                return Ok(());
            }

            let event = if shutdown_open {
                tokio::select! {
                    biased;
                    changed = shutdown.changed() => {
                        if changed.is_err() {
                            shutdown_open = false;
                        }
                        continue;
                    }
                    event = self.receiver.recv() => event,
                }
            } else {
                self.receiver.recv().await
            };
            let Some(event) = event else {
                return Ok(());
            };

            let payload = match serde_json::to_string(&event) {
                Ok(payload) => payload,
                Err(error) => {
                    self.record_abandoned(1).await;
                    return Err(RedisError::from((
                        redis::ErrorKind::TypeError,
                        "request metadata event serialization failed",
                        error.to_string(),
                    )));
                }
            };
            let mut backoff = Duration::from_millis(25);
            loop {
                let mut command = redis::cmd("XADD");
                command.arg(stream).arg("*").arg("event").arg(&payload);
                let write = command.query_async(&mut connection);
                let result: Result<String, RedisError> =
                    match tokio::time::timeout(STREAM_WRITE_TIMEOUT, write).await {
                        Ok(result) => result,
                        Err(_) => Err(RedisError::from((
                            redis::ErrorKind::IoError,
                            "request metadata stream write timed out",
                        ))),
                    };
                match result {
                    Ok(_) => {
                        self.health
                            .persisted
                            .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                        break;
                    }
                    Err(_) => {
                        self.health
                            .retrying
                            .store(true, std::sync::atomic::Ordering::Relaxed);
                        if *shutdown.borrow() {
                            self.record_abandoned(1).await;
                            return Ok(());
                        }
                        if shutdown_open {
                            tokio::select! {
                                () = tokio::time::sleep(backoff) => {}
                                changed = shutdown.changed() => {
                                    if changed.is_err() {
                                        shutdown_open = false;
                                    } else if *shutdown.borrow() {
                                        self.record_abandoned(1).await;
                                        return Ok(());
                                    }
                                }
                            }
                        } else {
                            tokio::time::sleep(backoff).await;
                        }
                        backoff = (backoff * 2).min(Duration::from_secs(5));
                    }
                }
            }
            self.health
                .retrying
                .store(false, std::sync::atomic::Ordering::Relaxed);
        }
    }

    /// Stops new reservations and drains until outstanding permits are either
    /// published or released, ensuring every accepted event is accounted for.
    pub(super) async fn record_abandoned(&mut self, current_event_count: u64) {
        self.receiver.close();
        let mut abandoned = current_event_count;
        while self.receiver.recv().await.is_some() {
            abandoned = abandoned.saturating_add(1);
        }
        self.health.record_abandoned(abandoned);
    }
}

pub(super) struct RequestMetadataBufferHealth {
    process_epoch: Uuid,
    started_at_ms: i64,
    pub(super) accepted: std::sync::atomic::AtomicU64,
    pub(super) persisted: std::sync::atomic::AtomicU64,
    dropped: std::sync::atomic::AtomicU64,
    abandoned: std::sync::atomic::AtomicU64,
    retrying: std::sync::atomic::AtomicBool,
    first_loss_at_ms: std::sync::atomic::AtomicI64,
    last_loss_at_ms: std::sync::atomic::AtomicI64,
}

impl Default for RequestMetadataBufferHealth {
    fn default() -> Self {
        Self {
            process_epoch: Uuid::now_v7(),
            started_at_ms: Utc::now().timestamp_millis(),
            accepted: std::sync::atomic::AtomicU64::new(0),
            persisted: std::sync::atomic::AtomicU64::new(0),
            dropped: std::sync::atomic::AtomicU64::new(0),
            abandoned: std::sync::atomic::AtomicU64::new(0),
            retrying: std::sync::atomic::AtomicBool::new(false),
            first_loss_at_ms: std::sync::atomic::AtomicI64::new(0),
            last_loss_at_ms: std::sync::atomic::AtomicI64::new(0),
        }
    }
}

impl RequestMetadataBufferHealth {
    fn record_dropped(&self, count: u64) {
        self.dropped
            .fetch_add(count, std::sync::atomic::Ordering::Relaxed);
        self.record_loss_time(count);
    }

    fn record_abandoned(&self, count: u64) {
        self.abandoned
            .fetch_add(count, std::sync::atomic::Ordering::SeqCst);
        self.record_loss_time(count);
    }

    fn record_loss_time(&self, count: u64) {
        if count == 0 {
            return;
        }
        let now = Utc::now().timestamp_millis();
        let _ = self.first_loss_at_ms.compare_exchange(
            0,
            now,
            std::sync::atomic::Ordering::Relaxed,
            std::sync::atomic::Ordering::Relaxed,
        );
        self.last_loss_at_ms
            .store(now, std::sync::atomic::Ordering::Relaxed);
    }

    pub(super) fn snapshot(&self) -> RequestMetadataBufferSnapshot {
        // Downstream counts can never precede acceptance, but retain the lower
        // bound as a fail-closed guard against impossible durable checkpoints.
        let persisted = self.persisted.load(std::sync::atomic::Ordering::SeqCst);
        let abandoned = self.abandoned.load(std::sync::atomic::Ordering::SeqCst);
        let accepted = self
            .accepted
            .load(std::sync::atomic::Ordering::SeqCst)
            .max(persisted.saturating_add(abandoned));
        RequestMetadataBufferSnapshot {
            process_epoch: self.process_epoch,
            started_at: timestamp_millis(self.started_at_ms).unwrap_or_else(Utc::now),
            accepted,
            persisted,
            dropped: self.dropped.load(std::sync::atomic::Ordering::Relaxed),
            abandoned,
            retrying: self.retrying.load(std::sync::atomic::Ordering::Relaxed),
            closed: false,
            first_loss_at: timestamp_millis(
                self.first_loss_at_ms
                    .load(std::sync::atomic::Ordering::Relaxed),
            ),
            last_loss_at: timestamp_millis(
                self.last_loss_at_ms
                    .load(std::sync::atomic::Ordering::Relaxed),
            ),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RequestMetadataBufferSnapshot {
    /// Distinguishes counter resets after a gateway process restart.
    pub process_epoch: Uuid,
    pub started_at: DateTime<Utc>,
    pub accepted: u64,
    pub persisted: u64,
    /// Events rejected before entering the bounded queue.
    pub dropped: u64,
    /// Events accepted into the queue but lost when the worker stopped.
    pub abandoned: u64,
    pub retrying: bool,
    /// The local stream writer has stopped and cannot accept more events.
    pub closed: bool,
    pub first_loss_at: Option<DateTime<Utc>>,
    pub last_loss_at: Option<DateTime<Utc>>,
}

impl RequestMetadataBufferSnapshot {
    pub fn complete(&self) -> bool {
        self.dropped == 0 && self.abandoned == 0 && !self.retrying && !self.closed
    }

    pub fn pending(&self) -> u64 {
        self.accepted
            .saturating_sub(self.persisted.saturating_add(self.abandoned))
    }

    pub fn lost(&self) -> u64 {
        self.dropped.saturating_add(self.abandoned)
    }

    #[must_use]
    pub fn gracefully_drained(&self) -> bool {
        self.closed && self.pending() == 0
    }
}

fn timestamp_millis(value: i64) -> Option<DateTime<Utc>> {
    (value > 0)
        .then(|| DateTime::<Utc>::from_timestamp_millis(value))
        .flatten()
}
