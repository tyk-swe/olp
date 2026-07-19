use std::{sync::Arc, time::Duration};

use chrono::{DateTime, Utc};
use olp_domain::{OperationKind, Surface};
use redis::{Client, RedisError, aio::ConnectionManager};
use rust_decimal::Decimal;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use sqlx::{Postgres, QueryBuilder, Row};
use thiserror::Error;
use tokio::sync::{mpsc, watch};
use uuid::Uuid;

use crate::{OperationsError, Page, PersistenceError, PgStore, TimestampCursor, split_page};

const STREAM_WRITE_TIMEOUT: Duration = Duration::from_secs(1);
const USAGE_GATEWAY_EPOCH_LOCK_SEED: i64 = 0x4f4c_505f_5545;
pub(crate) const USAGE_EVENT_REPLAY_HORIZON_DAYS: i64 = 7;
pub(crate) const USAGE_EVENT_FUTURE_SKEW_MINUTES: i64 = 5;

/// Metadata-only usage envelope. Content-bearing fields do not exist in this
/// type, making accidental prompt/output persistence structurally impossible.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct UsageEvent {
    pub event_id: Uuid,
    pub request_id: Uuid,
    pub runtime_generation_id: Uuid,
    pub api_key_id: Uuid,
    /// Absent when an authenticated request fails before a provider attempt can
    /// be selected. Such events still produce request metadata, but never a
    /// usage fact.
    pub provider_id: Option<Uuid>,
    pub route_slug: String,
    pub upstream_model: Option<String>,
    pub operation: OperationKind,
    pub surface: Surface,
    pub request_started_at: DateTime<Utc>,
    pub request_completed_at: DateTime<Utc>,
    pub observed_at: DateTime<Utc>,
    pub status_code: Option<u16>,
    pub error_class: Option<String>,
    pub committed: bool,
    pub latency_ms: u64,
    pub first_byte_ms: Option<u64>,
    pub input_tokens: Option<i64>,
    pub output_tokens: Option<i64>,
    pub cached_input_tokens: Option<i64>,
    pub media_units: Option<Decimal>,
    pub usage_complete: bool,
    pub unpriced: bool,
    pub attempts: Vec<UsageAttempt>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct UsageAttempt {
    pub id: Uuid,
    pub ordinal: u16,
    pub provider_id: Uuid,
    pub upstream_model: String,
    pub started_at: DateTime<Utc>,
    pub completed_at: DateTime<Utc>,
    pub status_code: Option<u16>,
    pub error_class: Option<String>,
    pub committed: bool,
    pub latency_ms: u64,
    pub first_byte_ms: Option<u64>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum UsagePersistenceOutcome {
    Persisted,
    Duplicate,
    RejectedOutsideReplayWindow,
}

#[derive(Clone)]
pub struct UsageEmitter {
    sender: mpsc::Sender<UsageEvent>,
    health: Arc<UsageBufferHealth>,
}

impl UsageEmitter {
    pub fn bounded(capacity: usize) -> (Self, UsageReceiver) {
        let (sender, receiver) = mpsc::channel(capacity.max(1));
        let health = Arc::new(UsageBufferHealth::default());
        (
            Self {
                sender,
                health: Arc::clone(&health),
            },
            UsageReceiver { receiver, health },
        )
    }

    /// Never blocks the inference response path. Overflow is counted and made
    /// visible; callers must include this counter in readiness and metrics.
    pub fn emit(&self, event: UsageEvent) -> Result<(), UsageEmitError> {
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
                Err(UsageEmitError::Full)
            }
            Err(mpsc::error::TrySendError::Closed(_)) => {
                self.health.record_dropped(1);
                Err(UsageEmitError::Closed)
            }
        }
    }

    pub fn snapshot(&self) -> UsageBufferSnapshot {
        let mut snapshot = self.health.snapshot();
        snapshot.closed = self.sender.is_closed();
        snapshot
    }
}

#[derive(Debug, Clone, Copy, Error, PartialEq, Eq)]
pub enum UsageEmitError {
    #[error("the bounded usage buffer is full")]
    Full,
    #[error("the usage persistence worker is not running")]
    Closed,
}

pub struct UsageReceiver {
    receiver: mpsc::Receiver<UsageEvent>,
    health: Arc<UsageBufferHealth>,
}

impl UsageReceiver {
    /// Receives one buffered event. This supports alternate persistence
    /// adapters and deterministic integration tests without exposing prompts
    /// or response bodies (which are absent from [`UsageEvent`]).
    pub async fn recv_next(&mut self) -> Option<UsageEvent> {
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
    ) -> Result<(), RedisError> {
        let client = match Client::open(valkey_url) {
            Ok(client) => client,
            Err(error) => {
                self.record_abandoned(0).await;
                return Err(error);
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
            match connection {
                Ok(connection) => {
                    self.health
                        .retrying
                        .store(false, std::sync::atomic::Ordering::Relaxed);
                    return self.run(connection, stream, shutdown).await;
                }
                Err(_) => {
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
        }
    }

    /// Writes to a Valkey Stream with bounded local buffering. On an outage the
    /// current event is retried, the channel fills to its configured bound, and
    /// further loss is explicitly counted by `UsageEmitter`.
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
                        "usage event serialization failed",
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
                            "usage stream write timed out",
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
    async fn record_abandoned(&mut self, current_event_count: u64) {
        self.receiver.close();
        let mut abandoned = current_event_count;
        while self.receiver.recv().await.is_some() {
            abandoned = abandoned.saturating_add(1);
        }
        self.health.record_abandoned(abandoned);
    }
}

struct UsageBufferHealth {
    process_epoch: Uuid,
    started_at_ms: i64,
    accepted: std::sync::atomic::AtomicU64,
    persisted: std::sync::atomic::AtomicU64,
    dropped: std::sync::atomic::AtomicU64,
    abandoned: std::sync::atomic::AtomicU64,
    retrying: std::sync::atomic::AtomicBool,
    first_loss_at_ms: std::sync::atomic::AtomicI64,
    last_loss_at_ms: std::sync::atomic::AtomicI64,
}

impl Default for UsageBufferHealth {
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

impl UsageBufferHealth {
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

    fn snapshot(&self) -> UsageBufferSnapshot {
        // Downstream counts can never precede acceptance, but retain the lower
        // bound as a fail-closed guard against impossible durable checkpoints.
        let persisted = self.persisted.load(std::sync::atomic::Ordering::SeqCst);
        let abandoned = self.abandoned.load(std::sync::atomic::Ordering::SeqCst);
        let accepted = self
            .accepted
            .load(std::sync::atomic::Ordering::SeqCst)
            .max(persisted.saturating_add(abandoned));
        UsageBufferSnapshot {
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
pub struct UsageBufferSnapshot {
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

impl UsageBufferSnapshot {
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

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct UsageLossReport {
    pub reported_events: u64,
    pub reported_dropped: u64,
    pub reported_abandoned: u64,
    pub process_epoch_changed: bool,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct UsageEpochDetection {
    pub candidate_epochs: u64,
    pub detected_epochs: u64,
    pub uncertain_event_lower_bound: u64,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct UsageEpochHealth {
    pub open_epochs: u64,
    pub unresolved_epochs: u64,
    pub historical_uncertain_gap_count: u64,
    pub unresolved_event_lower_bound: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct UsageEpochAcknowledgement {
    pub gateway_instance: String,
    pub process_epoch: Uuid,
    pub acknowledged_at: DateTime<Utc>,
    pub acknowledged_by: Option<Uuid>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum UsageGatewayEpochState {
    Open,
    GracefullyClosed,
    Unresolved,
    Acknowledged,
}

impl UsageGatewayEpochState {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Open => "open",
            Self::GracefullyClosed => "gracefully_closed",
            Self::Unresolved => "unresolved",
            Self::Acknowledged => "acknowledged",
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct UsageGatewayEpochRecord {
    pub gateway_instance: String,
    pub process_epoch: Uuid,
    pub state: UsageGatewayEpochState,
    pub started_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub accepted: u64,
    pub persisted: u64,
    pub dropped: u64,
    pub abandoned: u64,
    pub uncertain_event_lower_bound: u64,
    pub retrying: bool,
    pub writer_closed: bool,
    pub gracefully_closed_at: Option<DateTime<Utc>>,
    pub stale_detected_at: Option<DateTime<Utc>>,
    pub acknowledged_at: Option<DateTime<Utc>>,
    pub acknowledged_by: Option<Uuid>,
}

/// Gateway-local checkpoints are emitted every second. A minute without a
/// checkpoint followed by a separate confirmation pass avoids fabricating an
/// outage when PostgreSQL briefly recovers before the live gateway reporter.
pub const USAGE_GATEWAY_EPOCH_STALE_AFTER_SECONDS: i64 = 60;
const USAGE_GATEWAY_EPOCH_CONFIRM_AFTER_SECONDS: i64 = 10;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct UsageConsumerHealth {
    pub pending_events: u64,
    pub lag_events: u64,
    pub oldest_pending_at: Option<DateTime<Utc>>,
    pub checked_at: DateTime<Utc>,
}

/// The worker reports every five seconds. Four missed checkpoints distinguish
/// a genuinely stale consumer from ordinary scheduling and database jitter.
pub const USAGE_CONSUMER_STALE_AFTER_SECONDS: i64 = 20;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum UsageConsumerState {
    Unknown,
    Healthy,
    Backlogged,
    Stale,
}

impl UsageConsumerState {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Unknown => "unknown",
            Self::Healthy => "healthy",
            Self::Backlogged => "backlogged",
            Self::Stale => "stale",
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct UsageConsumerStatus {
    pub state: UsageConsumerState,
    pub pending_events: u64,
    pub lag_events: u64,
    pub oldest_pending_at: Option<DateTime<Utc>>,
    pub checked_at: Option<DateTime<Utc>>,
    pub heartbeat_age_seconds: Option<u64>,
}

impl UsageConsumerStatus {
    #[must_use]
    pub fn from_health(health: Option<UsageConsumerHealth>, now: DateTime<Utc>) -> Self {
        let Some(health) = health else {
            return Self {
                state: UsageConsumerState::Unknown,
                pending_events: 0,
                lag_events: 0,
                oldest_pending_at: None,
                checked_at: None,
                heartbeat_age_seconds: None,
            };
        };
        let age_seconds = u64::try_from(
            now.signed_duration_since(health.checked_at)
                .num_seconds()
                .max(0),
        )
        .unwrap_or(u64::MAX);
        let state = if age_seconds
            > u64::try_from(USAGE_CONSUMER_STALE_AFTER_SECONDS)
                .expect("the consumer stale threshold is positive")
        {
            UsageConsumerState::Stale
        } else if health.pending_events > 0 || health.lag_events > 0 {
            UsageConsumerState::Backlogged
        } else {
            UsageConsumerState::Healthy
        };
        Self {
            state,
            pending_events: health.pending_events,
            lag_events: health.lag_events,
            oldest_pending_at: health.oldest_pending_at,
            checked_at: Some(health.checked_at),
            heartbeat_age_seconds: Some(age_seconds),
        }
    }

    #[must_use]
    pub const fn complete(self) -> bool {
        matches!(self.state, UsageConsumerState::Healthy)
    }
}

fn timestamp_millis(value: i64) -> Option<DateTime<Utc>> {
    (value > 0)
        .then(|| DateTime::<Utc>::from_timestamp_millis(value))
        .flatten()
}

async fn record_unclean_usage_epoch(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    gateway_instance: &str,
    row: &sqlx::postgres::PgRow,
    detected_at: DateTime<Utc>,
) -> Result<i64, PersistenceError> {
    let process_epoch: Uuid = row.get("process_epoch");
    let accepted: i64 = row.get("accepted");
    let persisted: i64 = row.get("persisted");
    let abandoned: i64 = row.get("abandoned");
    let last_checkpoint: DateTime<Utc> = row.get("updated_at");
    let lower_bound = accepted
        .saturating_sub(persisted)
        .saturating_sub(abandoned)
        .max(0);
    let gap_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO usage_ingestion_gaps \
         (id, gateway_instance, event_count, reason, certainty, first_observed_at, \
          last_observed_at, reported_at) \
         VALUES ($1, $2, $3, 'gateway_epoch_unclean_shutdown', \
                 'lower_bound'::usage_gap_certainty, $4, $5, $5)",
    )
    .bind(gap_id)
    .bind(gateway_instance)
    .bind(lower_bound)
    .bind(last_checkpoint)
    .bind(detected_at.max(last_checkpoint))
    .execute(&mut **transaction)
    .await?;
    sqlx::query(
        "UPDATE usage_gateway_epochs \
         SET stale_detected_at = $1, uncertainty_gap_id = $2 \
         WHERE gateway_instance = $3 AND process_epoch = $4 \
           AND gracefully_closed_at IS NULL AND stale_detected_at IS NULL",
    )
    .bind(detected_at)
    .bind(gap_id)
    .bind(gateway_instance)
    .bind(process_epoch)
    .execute(&mut **transaction)
    .await?;
    Ok(lower_bound)
}

fn usage_gateway_epoch_from_row(
    row: sqlx::postgres::PgRow,
) -> Result<UsageGatewayEpochRecord, OperationsError> {
    let gracefully_closed_at: Option<DateTime<Utc>> = row.get("gracefully_closed_at");
    let stale_detected_at: Option<DateTime<Utc>> = row.get("stale_detected_at");
    let acknowledged_at: Option<DateTime<Utc>> = row.get("acknowledged_at");
    let state = if gracefully_closed_at.is_some() {
        UsageGatewayEpochState::GracefullyClosed
    } else if stale_detected_at.is_some() && acknowledged_at.is_some() {
        UsageGatewayEpochState::Acknowledged
    } else if stale_detected_at.is_some() {
        UsageGatewayEpochState::Unresolved
    } else {
        UsageGatewayEpochState::Open
    };
    let checked_count = |column| {
        u64::try_from(row.get::<i64, _>(column))
            .map_err(|_| OperationsError::Persistence(PersistenceError::InvalidUsageGap))
    };
    Ok(UsageGatewayEpochRecord {
        gateway_instance: row.get("gateway_instance"),
        process_epoch: row.get("process_epoch"),
        state,
        started_at: row.get("started_at"),
        updated_at: row.get("updated_at"),
        accepted: checked_count("accepted")?,
        persisted: checked_count("persisted")?,
        dropped: checked_count("dropped")?,
        abandoned: checked_count("abandoned")?,
        uncertain_event_lower_bound: checked_count("uncertain_lower_bound")?,
        retrying: row.get("retrying"),
        writer_closed: row.get("writer_closed"),
        gracefully_closed_at,
        stale_detected_at,
        acknowledged_at,
        acknowledged_by: row.get("acknowledged_by"),
    })
}

impl PgStore {
    /// Checkpoints the Valkey consumer-group backlog so health and usage
    /// completeness reflect worker-side stalls, not only gateway-local queue
    /// delivery. This contains counts and timestamps only.
    pub async fn report_usage_consumer_health(
        &self,
        pending_events: u64,
        lag_events: u64,
        oldest_pending_at: Option<DateTime<Utc>>,
    ) -> Result<UsageConsumerHealth, PersistenceError> {
        if (pending_events == 0) != oldest_pending_at.is_none() {
            return Err(PersistenceError::InvalidUsageGap);
        }
        let pending_events =
            i64::try_from(pending_events).map_err(|_| PersistenceError::InvalidUsageGap)?;
        let lag_events =
            i64::try_from(lag_events).map_err(|_| PersistenceError::InvalidUsageGap)?;
        let checked_at = Utc::now();
        if oldest_pending_at
            .is_some_and(|oldest| oldest > checked_at + chrono::Duration::minutes(5))
        {
            return Err(PersistenceError::InvalidUsageGap);
        }
        let row = sqlx::query(
            "INSERT INTO usage_consumer_health \
             (singleton, pending_events, lag_events, oldest_pending_at, checked_at) \
             VALUES (true, $1, $2, $3, $4) \
             ON CONFLICT (singleton) DO UPDATE SET \
               pending_events = EXCLUDED.pending_events, \
               lag_events = EXCLUDED.lag_events, \
               oldest_pending_at = EXCLUDED.oldest_pending_at, \
               checked_at = EXCLUDED.checked_at \
             RETURNING pending_events, lag_events, oldest_pending_at, checked_at",
        )
        .bind(pending_events)
        .bind(lag_events)
        .bind(oldest_pending_at)
        .bind(checked_at)
        .fetch_one(self.pool())
        .await?;
        Ok(UsageConsumerHealth {
            pending_events: u64::try_from(row.get::<i64, _>("pending_events"))
                .map_err(|_| PersistenceError::InvalidUsageGap)?,
            lag_events: u64::try_from(row.get::<i64, _>("lag_events"))
                .map_err(|_| PersistenceError::InvalidUsageGap)?,
            oldest_pending_at: row.get("oldest_pending_at"),
            checked_at: row.get("checked_at"),
        })
    }

    pub async fn usage_consumer_health(
        &self,
    ) -> Result<Option<UsageConsumerHealth>, PersistenceError> {
        let row = sqlx::query(
            "SELECT pending_events, lag_events, oldest_pending_at, checked_at \
             FROM usage_consumer_health WHERE singleton",
        )
        .fetch_optional(self.pool())
        .await?;
        row.map(|row| {
            Ok(UsageConsumerHealth {
                pending_events: u64::try_from(row.get::<i64, _>("pending_events"))
                    .map_err(|_| PersistenceError::InvalidUsageGap)?,
                lag_events: u64::try_from(row.get::<i64, _>("lag_events"))
                    .map_err(|_| PersistenceError::InvalidUsageGap)?,
                oldest_pending_at: row.get("oldest_pending_at"),
                checked_at: row.get("checked_at"),
            })
        })
        .transpose()
    }

    pub async fn usage_consumer_status(
        &self,
        now: DateTime<Utc>,
    ) -> Result<UsageConsumerStatus, PersistenceError> {
        Ok(UsageConsumerStatus::from_health(
            self.usage_consumer_health().await?,
            now,
        ))
    }

    /// Durably checkpoints the current gateway epoch and records exact unseen
    /// local-loss deltas. This runs only in the background reporter; inference
    /// requests never wait for PostgreSQL.
    pub async fn report_usage_buffer_loss(
        &self,
        gateway_instance: &str,
        snapshot: &UsageBufferSnapshot,
    ) -> Result<UsageLossReport, PersistenceError> {
        self.checkpoint_usage_buffer_epoch(gateway_instance, snapshot, false)
            .await
    }

    /// Marks an epoch as gracefully closed after the stream writer has closed
    /// its receiver and accounted for every accepted-but-unpersisted event.
    /// A clean close is never eligible for stale-epoch uncertainty detection.
    pub async fn close_usage_buffer_epoch(
        &self,
        gateway_instance: &str,
        snapshot: &UsageBufferSnapshot,
    ) -> Result<UsageLossReport, PersistenceError> {
        self.checkpoint_usage_buffer_epoch(gateway_instance, snapshot, true)
            .await
    }

    async fn checkpoint_usage_buffer_epoch(
        &self,
        gateway_instance: &str,
        snapshot: &UsageBufferSnapshot,
        graceful_close: bool,
    ) -> Result<UsageLossReport, PersistenceError> {
        let gateway_instance = gateway_instance.trim();
        if gateway_instance.is_empty()
            || gateway_instance.len() > 200
            || gateway_instance.chars().any(char::is_control)
            || snapshot.persisted > snapshot.accepted
            || snapshot.abandoned > snapshot.accepted - snapshot.persisted
            || (graceful_close && !snapshot.gracefully_drained())
        {
            return Err(PersistenceError::InvalidUsageGap);
        }
        let accepted =
            i64::try_from(snapshot.accepted).map_err(|_| PersistenceError::InvalidUsageGap)?;
        let persisted =
            i64::try_from(snapshot.persisted).map_err(|_| PersistenceError::InvalidUsageGap)?;
        let dropped =
            i64::try_from(snapshot.dropped).map_err(|_| PersistenceError::InvalidUsageGap)?;
        let abandoned =
            i64::try_from(snapshot.abandoned).map_err(|_| PersistenceError::InvalidUsageGap)?;
        let now = Utc::now();
        let mut transaction = self.pool().begin().await?;
        sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, $2))")
            .bind(gateway_instance)
            .bind(USAGE_GATEWAY_EPOCH_LOCK_SEED)
            .execute(&mut *transaction)
            .await?;
        let previous = sqlx::query(
            "SELECT accepted, persisted, dropped, abandoned, writer_closed, updated_at, \
                    gracefully_closed_at, stale_detected_at \
             FROM usage_gateway_epochs \
             WHERE gateway_instance = $1 AND process_epoch = $2 FOR UPDATE",
        )
        .bind(gateway_instance)
        .bind(snapshot.process_epoch)
        .fetch_optional(&mut *transaction)
        .await?;
        let process_epoch_changed = previous.is_none();
        if previous.is_none() {
            // A replacement epoch with the same stable gateway identity proves
            // that every older unclosed epoch ended without its shutdown hook.
            let superseded = sqlx::query(
                "SELECT process_epoch, accepted, persisted, abandoned, updated_at \
                 FROM usage_gateway_epochs \
                 WHERE gateway_instance = $1 AND process_epoch <> $2 \
                   AND gracefully_closed_at IS NULL AND stale_detected_at IS NULL \
                 FOR UPDATE",
            )
            .bind(gateway_instance)
            .bind(snapshot.process_epoch)
            .fetch_all(&mut *transaction)
            .await?;
            for epoch in superseded {
                record_unclean_usage_epoch(&mut transaction, gateway_instance, &epoch, now).await?;
            }
        }
        let (previous_dropped, previous_abandoned, previous_checkpoint) =
            if let Some(row) = previous {
                if row
                    .get::<Option<DateTime<Utc>>, _>("stale_detected_at")
                    .is_some()
                {
                    return Err(PersistenceError::InvalidUsageGap);
                }
                let previous_accepted = row.get::<i64, _>("accepted");
                let previous_persisted = row.get::<i64, _>("persisted");
                let previous_dropped = row.get::<i64, _>("dropped");
                let previous_abandoned = row.get::<i64, _>("abandoned");
                let previously_closed = row.get::<bool, _>("writer_closed");
                if accepted < previous_accepted
                    || persisted < previous_persisted
                    || dropped < previous_dropped
                    || abandoned < previous_abandoned
                    || (previously_closed && !snapshot.closed)
                {
                    return Err(PersistenceError::InvalidUsageGap);
                }
                if row
                    .get::<Option<DateTime<Utc>>, _>("gracefully_closed_at")
                    .is_some()
                {
                    if graceful_close
                        && snapshot.closed
                        && accepted == previous_accepted
                        && persisted == previous_persisted
                        && dropped == previous_dropped
                        && abandoned == previous_abandoned
                    {
                        transaction.commit().await?;
                        return Ok(UsageLossReport {
                            reported_events: 0,
                            reported_dropped: 0,
                            reported_abandoned: 0,
                            process_epoch_changed: false,
                        });
                    }
                    return Err(PersistenceError::InvalidUsageGap);
                }
                (
                    previous_dropped,
                    previous_abandoned,
                    row.get::<DateTime<Utc>, _>("updated_at"),
                )
            } else {
                (0_i64, 0_i64, snapshot.started_at)
            };
        let dropped_delta = dropped - previous_dropped;
        let abandoned_delta = abandoned - previous_abandoned;
        let event_count = dropped_delta
            .checked_add(abandoned_delta)
            .ok_or(PersistenceError::InvalidUsageGap)?;
        if event_count > 0 {
            let last_observed_at = snapshot.last_loss_at.unwrap_or(now);
            let first_observed_at = if process_epoch_changed {
                snapshot.first_loss_at.unwrap_or(snapshot.started_at)
            } else {
                snapshot
                    .first_loss_at
                    .map_or(previous_checkpoint, |first| first.max(previous_checkpoint))
            }
            .min(last_observed_at);
            sqlx::query(
                "INSERT INTO usage_ingestion_gaps \
                 (id, gateway_instance, event_count, reason, first_observed_at, \
                  last_observed_at, reported_at) \
                 VALUES ($1, $2, $3, 'gateway_local_buffer_loss', $4, $5, $6)",
            )
            .bind(Uuid::now_v7())
            .bind(gateway_instance)
            .bind(event_count)
            .bind(first_observed_at)
            .bind(last_observed_at)
            .bind(now)
            .execute(&mut *transaction)
            .await?;
        }
        sqlx::query(
            "INSERT INTO usage_gateway_epochs \
             (gateway_instance, process_epoch, started_at, accepted, persisted, dropped, abandoned, \
              retrying, writer_closed, updated_at, gracefully_closed_at) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, \
                     CASE WHEN $11 THEN $10 ELSE NULL END) \
             ON CONFLICT (gateway_instance, process_epoch) DO UPDATE SET \
               accepted = EXCLUDED.accepted, persisted = EXCLUDED.persisted, \
               dropped = EXCLUDED.dropped, abandoned = EXCLUDED.abandoned, \
               retrying = EXCLUDED.retrying, writer_closed = EXCLUDED.writer_closed, \
               updated_at = EXCLUDED.updated_at, stale_candidate_at = NULL, \
               gracefully_closed_at = CASE WHEN $11 \
                 THEN COALESCE(usage_gateway_epochs.gracefully_closed_at, $10) \
                 ELSE usage_gateway_epochs.gracefully_closed_at END",
        )
        .bind(gateway_instance)
        .bind(snapshot.process_epoch)
        .bind(snapshot.started_at)
        .bind(accepted)
        .bind(persisted)
        .bind(dropped)
        .bind(abandoned)
        .bind(snapshot.retrying)
        .bind(snapshot.closed)
        .bind(now)
        .bind(graceful_close)
        .execute(&mut *transaction)
        .await?;
        // Keep the pre-epoch cumulative checkpoint current during rolling
        // upgrades and rollback. New code never reads it for epoch detection.
        sqlx::query(
            "INSERT INTO usage_loss_reporter_state \
             (gateway_instance, process_epoch, dropped, abandoned, updated_at) \
             VALUES ($1, $2, $3, $4, $5) \
             ON CONFLICT (gateway_instance) DO UPDATE SET \
               process_epoch = EXCLUDED.process_epoch, dropped = EXCLUDED.dropped, \
               abandoned = EXCLUDED.abandoned, updated_at = EXCLUDED.updated_at",
        )
        .bind(gateway_instance)
        .bind(snapshot.process_epoch)
        .bind(dropped)
        .bind(abandoned)
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        Ok(UsageLossReport {
            reported_events: u64::try_from(event_count)
                .map_err(|_| PersistenceError::InvalidUsageGap)?,
            reported_dropped: u64::try_from(dropped_delta)
                .map_err(|_| PersistenceError::InvalidUsageGap)?,
            reported_abandoned: u64::try_from(abandoned_delta)
                .map_err(|_| PersistenceError::InvalidUsageGap)?,
            process_epoch_changed,
        })
    }

    /// Two-pass stale detection makes abrupt loss durable and idempotent. The
    /// first pass records a candidate; a later pass confirms it only if no live
    /// reporter cleared the marker with another checkpoint.
    pub async fn detect_stale_usage_gateway_epochs(
        &self,
        now: DateTime<Utc>,
    ) -> Result<UsageEpochDetection, PersistenceError> {
        let stale_cutoff = now - chrono::Duration::seconds(USAGE_GATEWAY_EPOCH_STALE_AFTER_SECONDS);
        let confirmation_cutoff =
            now - chrono::Duration::seconds(USAGE_GATEWAY_EPOCH_CONFIRM_AFTER_SECONDS);
        let mut transaction = self.pool().begin().await?;
        let rows = sqlx::query(
            "SELECT gateway_instance, process_epoch, accepted, persisted, abandoned, updated_at, \
                    stale_candidate_at \
             FROM usage_gateway_epochs \
             WHERE gracefully_closed_at IS NULL AND stale_detected_at IS NULL \
               AND updated_at < $1 \
             ORDER BY updated_at, gateway_instance, process_epoch \
             LIMIT 100 FOR UPDATE SKIP LOCKED",
        )
        .bind(stale_cutoff)
        .fetch_all(&mut *transaction)
        .await?;
        let mut report = UsageEpochDetection::default();
        for row in rows {
            let gateway_instance: String = row.get("gateway_instance");
            let process_epoch: Uuid = row.get("process_epoch");
            match row.get::<Option<DateTime<Utc>>, _>("stale_candidate_at") {
                Some(candidate_at) if candidate_at <= confirmation_cutoff => {
                    let lower_bound =
                        record_unclean_usage_epoch(&mut transaction, &gateway_instance, &row, now)
                            .await?;
                    report.detected_epochs = report.detected_epochs.saturating_add(1);
                    report.uncertain_event_lower_bound = report
                        .uncertain_event_lower_bound
                        .saturating_add(u64::try_from(lower_bound).unwrap_or(u64::MAX));
                }
                None => {
                    sqlx::query(
                        "UPDATE usage_gateway_epochs SET stale_candidate_at = $1 \
                         WHERE gateway_instance = $2 AND process_epoch = $3 \
                           AND gracefully_closed_at IS NULL AND stale_detected_at IS NULL",
                    )
                    .bind(now)
                    .bind(&gateway_instance)
                    .bind(process_epoch)
                    .execute(&mut *transaction)
                    .await?;
                    report.candidate_epochs = report.candidate_epochs.saturating_add(1);
                }
                Some(_) => {}
            }
        }
        transaction.commit().await?;
        Ok(report)
    }

    pub async fn usage_gateway_epoch_health(&self) -> Result<UsageEpochHealth, PersistenceError> {
        let row = sqlx::query(
            "SELECT count(*) FILTER (WHERE gracefully_closed_at IS NULL \
                                      AND stale_detected_at IS NULL) AS open_epochs, \
                    count(*) FILTER (WHERE stale_detected_at IS NOT NULL \
                                      AND acknowledged_at IS NULL) AS unresolved_epochs, \
                    COALESCE(sum(GREATEST(accepted - persisted - abandoned, 0)) \
                      FILTER (WHERE stale_detected_at IS NOT NULL \
                              AND acknowledged_at IS NULL), 0) AS unresolved_lower_bound, \
                    (SELECT count(*) FROM usage_ingestion_gaps \
                      WHERE certainty = 'lower_bound'::usage_gap_certainty) \
                    + (SELECT COALESCE(sum(uncertain_gap_count), 0) FROM usage_gap_hourly) \
                      AS historical_uncertain_gap_count \
             FROM usage_gateway_epochs",
        )
        .fetch_one(self.pool())
        .await?;
        let historical_uncertain_gap_count = usage_gap_count_from_decimal(
            row.try_get::<Decimal, _>("historical_uncertain_gap_count")?,
        )?;
        let unresolved_event_lower_bound =
            usage_gap_count_from_decimal(row.try_get::<Decimal, _>("unresolved_lower_bound")?)?;
        Ok(UsageEpochHealth {
            open_epochs: u64::try_from(row.get::<i64, _>("open_epochs"))
                .map_err(|_| PersistenceError::InvalidUsageGap)?,
            unresolved_epochs: u64::try_from(row.get::<i64, _>("unresolved_epochs"))
                .map_err(|_| PersistenceError::InvalidUsageGap)?,
            historical_uncertain_gap_count,
            unresolved_event_lower_bound,
        })
    }

    /// Lists metadata-only gateway process epochs for incident review. The
    /// cursor is ordered by the last durable checkpoint and UUIDv7 epoch ID.
    pub async fn usage_gateway_epochs(
        &self,
        state: Option<UsageGatewayEpochState>,
        cursor: Option<&TimestampCursor>,
        limit: u16,
    ) -> Result<Page<UsageGatewayEpochRecord>, OperationsError> {
        let page_size = limit.clamp(1, 200);
        let mut query = QueryBuilder::<Postgres>::new(
            "SELECT gateway_instance, process_epoch, started_at, updated_at, accepted, persisted, \
                    dropped, abandoned, retrying, writer_closed, gracefully_closed_at, \
                    stale_detected_at, acknowledged_at, acknowledged_by, \
                    CASE WHEN stale_detected_at IS NOT NULL \
                         THEN GREATEST(accepted - persisted - abandoned, 0) ELSE 0 END \
                      AS uncertain_lower_bound \
             FROM usage_gateway_epochs WHERE true",
        );
        match state {
            Some(UsageGatewayEpochState::Open) => {
                query.push(" AND gracefully_closed_at IS NULL AND stale_detected_at IS NULL");
            }
            Some(UsageGatewayEpochState::GracefullyClosed) => {
                query.push(" AND gracefully_closed_at IS NOT NULL");
            }
            Some(UsageGatewayEpochState::Unresolved) => {
                query.push(" AND stale_detected_at IS NOT NULL AND acknowledged_at IS NULL");
            }
            Some(UsageGatewayEpochState::Acknowledged) => {
                query.push(" AND stale_detected_at IS NOT NULL AND acknowledged_at IS NOT NULL");
            }
            None => {}
        }
        if let Some(cursor) = cursor {
            query.push(" AND (updated_at, process_epoch) < (");
            query.push_bind(cursor.at);
            query.push(", ");
            query.push_bind(cursor.id);
            query.push(")");
        }
        query.push(" ORDER BY updated_at DESC, process_epoch DESC LIMIT ");
        query.push_bind(i64::from(page_size) + 1);
        let rows = query.build().fetch_all(self.pool()).await?;
        let items = rows
            .into_iter()
            .map(usage_gateway_epoch_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        let (items, next_cursor) = split_page(items, usize::from(page_size), |item| {
            TimestampCursor {
                at: item.updated_at,
                id: item.process_epoch,
            }
            .encode()
        });
        Ok(Page { items, next_cursor })
    }

    /// Acknowledges durable uncertainty after an operator has reviewed the
    /// incident. This clears current readiness degradation but never deletes
    /// the raw/hourly completeness evidence for the affected time range.
    pub async fn acknowledge_usage_gateway_epoch(
        &self,
        process_epoch: Uuid,
        actor: Uuid,
    ) -> Result<Option<UsageEpochAcknowledgement>, PersistenceError> {
        let mut transaction = self.pool().begin().await?;
        let row = sqlx::query(
            "SELECT gateway_instance, acknowledged_at, acknowledged_by \
             FROM usage_gateway_epochs \
             WHERE process_epoch = $1 AND stale_detected_at IS NOT NULL \
             FOR UPDATE",
        )
        .bind(process_epoch)
        .fetch_optional(&mut *transaction)
        .await?;
        let Some(row) = row else {
            return Ok(None);
        };
        let gateway_instance: String = row.get("gateway_instance");
        let existing_at: Option<DateTime<Utc>> = row.get("acknowledged_at");
        let existing_actor: Option<Uuid> = row.get("acknowledged_by");
        if let Some(acknowledged_at) = existing_at {
            transaction.commit().await?;
            return Ok(Some(UsageEpochAcknowledgement {
                gateway_instance,
                process_epoch,
                acknowledged_at,
                acknowledged_by: existing_actor,
            }));
        }
        let acknowledged_at = Utc::now();
        let acknowledgement = sqlx::query(
            "UPDATE usage_gateway_epochs \
             SET acknowledged_at = GREATEST($1, stale_detected_at), acknowledged_by = $2 \
             WHERE process_epoch = $3 AND acknowledged_at IS NULL \
             RETURNING acknowledged_at, acknowledged_by",
        )
        .bind(acknowledged_at)
        .bind(actor)
        .bind(process_epoch)
        .fetch_one(&mut *transaction)
        .await?;
        let acknowledged_at: DateTime<Utc> = acknowledgement.get("acknowledged_at");
        let acknowledged_by: Option<Uuid> = acknowledgement.get("acknowledged_by");
        sqlx::query(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'usage.gateway_epoch_acknowledge', 'usage_gateway_epoch', \
                     $3, 'success', $4)",
        )
        .bind(Uuid::now_v7())
        .bind(actor)
        .bind(process_epoch.to_string())
        .bind(acknowledged_at)
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        Ok(Some(UsageEpochAcknowledgement {
            gateway_instance,
            process_epoch,
            acknowledged_at,
            acknowledged_by,
        }))
    }

    /// Persists one idempotent metadata-only stream event. A bounded durable
    /// receipt protects the supported seven-day delivery window after raw
    /// facts roll into hourly usage. Older entries are rejected explicitly so
    /// they cannot silently add to an aggregate after their receipt expires.
    pub async fn persist_usage_event(
        &self,
        event: &UsageEvent,
    ) -> Result<UsagePersistenceOutcome, PersistenceError> {
        let event_sha256: [u8; 32] = Sha256::digest(serde_json::to_vec(event)?).into();
        self.persist_usage_event_with_digest(event, event_sha256)
            .await
    }

    /// Processes an event decoded from a Valkey Stream while fingerprinting
    /// the original bytes. Replays therefore remain stable across application
    /// versions even if Rust's serialization of [`UsageEvent`] later changes.
    pub async fn persist_usage_stream_event(
        &self,
        event: &UsageEvent,
        original_payload: &[u8],
    ) -> Result<UsagePersistenceOutcome, PersistenceError> {
        let event_sha256: [u8; 32] = Sha256::digest(original_payload).into();
        self.persist_usage_event_with_digest(event, event_sha256)
            .await
    }

    async fn persist_usage_event_with_digest(
        &self,
        event: &UsageEvent,
        event_sha256: [u8; 32],
    ) -> Result<UsagePersistenceOutcome, PersistenceError> {
        let has_attempts = !event.attempts.is_empty();
        let final_target_matches = event.attempts.last().is_none_or(|attempt| {
            event.provider_id == Some(attempt.provider_id)
                && event.upstream_model.as_deref() == Some(attempt.upstream_model.as_str())
                && attempt.committed == event.committed
        });
        let empty_attempt_metadata_is_valid = has_attempts
            || (event.provider_id.is_none()
                && event.upstream_model.is_none()
                && !event.committed
                && event.first_byte_ms.is_none()
                && event.input_tokens.is_none()
                && event.output_tokens.is_none()
                && event.cached_input_tokens.is_none()
                && event.media_units.is_none()
                && !event.usage_complete);
        if event.request_completed_at < event.request_started_at
            || event.route_slug.trim().is_empty()
            || event
                .status_code
                .is_some_and(|status| !(100..=599).contains(&status))
            || !final_target_matches
            || !empty_attempt_metadata_is_valid
        {
            return Err(PersistenceError::InvalidUsageEvent);
        }
        let latency_ms =
            i32::try_from(event.latency_ms).map_err(|_| PersistenceError::InvalidUsageEvent)?;
        let first_byte_ms = event
            .first_byte_ms
            .map(i32::try_from)
            .transpose()
            .map_err(|_| PersistenceError::InvalidUsageEvent)?;
        let status_code = event.status_code.map(i32::from);
        let attempt_count =
            i16::try_from(event.attempts.len()).map_err(|_| PersistenceError::InvalidUsageEvent)?;
        for (index, attempt) in event.attempts.iter().enumerate() {
            if usize::from(attempt.ordinal) != index + 1
                || attempt.completed_at < attempt.started_at
                || attempt
                    .status_code
                    .is_some_and(|status| !(100..=599).contains(&status))
                || i32::try_from(attempt.latency_ms).is_err()
                || attempt
                    .first_byte_ms
                    .is_some_and(|value| i32::try_from(value).is_err())
            {
                return Err(PersistenceError::InvalidUsageEvent);
            }
        }
        let mut transaction = self.pool().begin().await?;
        let receipt: Option<Uuid> = sqlx::query_scalar(
            "INSERT INTO usage_event_receipts \
             (event_id, request_id, event_sha256, status, observed_at) \
             SELECT $1, $2, $3, 'pending'::usage_event_receipt_status, $4 \
             WHERE $4 >= now() - make_interval(days => $5) \
               AND $4 <= now() + make_interval(mins => $6) \
               AND NOT EXISTS (SELECT 1 FROM usage_facts \
                               WHERE id = $1 OR request_id = $2) \
             ON CONFLICT DO NOTHING RETURNING event_id",
        )
        .bind(event.event_id)
        .bind(event.request_id)
        .bind(event_sha256.as_slice())
        .bind(event.observed_at)
        .bind(i32::try_from(USAGE_EVENT_REPLAY_HORIZON_DAYS).expect("small replay horizon"))
        .bind(i32::try_from(USAGE_EVENT_FUTURE_SKEW_MINUTES).expect("small future skew"))
        .fetch_optional(&mut *transaction)
        .await?;
        if receipt.is_none() {
            let existing = sqlx::query(
                "SELECT \
                   EXISTS (SELECT 1 FROM usage_event_receipts \
                           WHERE event_id = $1 AND request_id = $2) AS receipt_exists, \
                   (SELECT event_sha256 FROM usage_event_receipts \
                    WHERE event_id = $1 AND request_id = $2) AS event_sha256, \
                   EXISTS (SELECT 1 FROM usage_facts \
                           WHERE id = $1 AND request_id = $2) AS fact_exists, \
                   ($3 < now() - make_interval(days => $4) \
                    OR $3 > now() + make_interval(mins => $5)) AS outside_window",
            )
            .bind(event.event_id)
            .bind(event.request_id)
            .bind(event.observed_at)
            .bind(i32::try_from(USAGE_EVENT_REPLAY_HORIZON_DAYS).expect("small replay horizon"))
            .bind(i32::try_from(USAGE_EVENT_FUTURE_SKEW_MINUTES).expect("small future skew"))
            .fetch_one(&mut *transaction)
            .await?;
            let receipt_exists: bool = existing.get("receipt_exists");
            let exact_receipt = receipt_exists
                && existing
                    .get::<Option<Vec<u8>>, _>("event_sha256")
                    .is_none_or(|stored| stored.as_slice() == event_sha256);
            let exact_raw_fact: bool = existing.get("fact_exists");
            if exact_receipt || exact_raw_fact {
                transaction.rollback().await?;
                return Ok(UsagePersistenceOutcome::Duplicate);
            }
            if existing.get::<bool, _>("outside_window") {
                let rejection: Option<Uuid> = sqlx::query_scalar(
                    "INSERT INTO usage_event_receipts \
                     (event_id, request_id, event_sha256, status, observed_at) \
                     SELECT $1, $2, $3, 'rejected'::usage_event_receipt_status, $4 \
                     WHERE NOT EXISTS (SELECT 1 FROM usage_facts \
                                       WHERE id = $1 OR request_id = $2) \
                     ON CONFLICT DO NOTHING RETURNING event_id",
                )
                .bind(event.event_id)
                .bind(event.request_id)
                .bind(event_sha256.as_slice())
                .bind(event.observed_at)
                .fetch_optional(&mut *transaction)
                .await?;
                if rejection.is_some() {
                    sqlx::query(
                        "INSERT INTO usage_ingestion_gaps \
                         (id, gateway_instance, event_count, reason, certainty, \
                          first_observed_at, last_observed_at) \
                         VALUES ($1, 'stream-consumer', 0, \
                                 'usage_event_outside_replay_window', \
                                 'lower_bound'::usage_gap_certainty, now(), now())",
                    )
                    .bind(Uuid::now_v7())
                    .execute(&mut *transaction)
                    .await?;
                    transaction.commit().await?;
                    return Ok(UsagePersistenceOutcome::RejectedOutsideReplayWindow);
                }
                let exact_after_race: bool = sqlx::query_scalar(
                    "SELECT EXISTS ( \
                       SELECT 1 FROM usage_event_receipts \
                       WHERE event_id = $1 AND request_id = $2 \
                         AND (event_sha256 IS NULL OR event_sha256 = $3) \
                       UNION ALL \
                       SELECT 1 FROM usage_facts WHERE id = $1 AND request_id = $2 \
                     )",
                )
                .bind(event.event_id)
                .bind(event.request_id)
                .bind(event_sha256.as_slice())
                .fetch_one(&mut *transaction)
                .await?;
                transaction.rollback().await?;
                return if exact_after_race {
                    Ok(UsagePersistenceOutcome::Duplicate)
                } else {
                    Err(PersistenceError::InvalidUsageEvent)
                };
            }
            transaction.rollback().await?;
            return Err(PersistenceError::InvalidUsageEvent);
        }
        sqlx::query(
            "INSERT INTO requests \
              (id, runtime_generation_id, api_key_id, route_slug, operation, surface, \
              started_at, completed_at, status_code, error_class, total_latency_ms, first_byte_ms, \
              attempt_count, created_at) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $8) \
             ON CONFLICT (id, started_at) DO NOTHING",
        )
        .bind(event.request_id)
        .bind(event.runtime_generation_id)
        .bind(event.api_key_id)
        .bind(&event.route_slug)
        .bind(event.operation.as_str())
        .bind(event.surface.as_str())
        .bind(event.request_started_at)
        .bind(event.request_completed_at)
        .bind(status_code)
        .bind(&event.error_class)
        .bind(latency_ms)
        .bind(first_byte_ms)
        .bind(attempt_count)
        .execute(&mut *transaction)
        .await?;
        for attempt in &event.attempts {
            let latency_ms = i32::try_from(attempt.latency_ms)
                .map_err(|_| PersistenceError::InvalidUsageEvent)?;
            let first_byte_ms = attempt
                .first_byte_ms
                .map(i32::try_from)
                .transpose()
                .map_err(|_| PersistenceError::InvalidUsageEvent)?;
            sqlx::query(
                "INSERT INTO attempts \
                 (id, request_id, request_started_at, ordinal, provider_id, upstream_model, \
                  started_at, completed_at, status_code, error_class, committed, latency_ms, first_byte_ms) \
                 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) \
                 ON CONFLICT (request_id, ordinal) DO NOTHING",
            )
            .bind(attempt.id)
            .bind(event.request_id)
            .bind(event.request_started_at)
            .bind(
                i16::try_from(attempt.ordinal)
                    .map_err(|_| PersistenceError::InvalidUsageEvent)?,
            )
            .bind(attempt.provider_id)
            .bind(&attempt.upstream_model)
            .bind(attempt.started_at)
            .bind(attempt.completed_at)
            .bind(attempt.status_code.map(i32::from))
            .bind(&attempt.error_class)
            .bind(attempt.committed)
            .bind(latency_ms)
            .bind(first_byte_ms)
            .execute(&mut *transaction)
            .await?;
        }

        // Authenticated decoding, route, and capability failures are valuable
        // operational metadata, but no provider usage exists to price or roll
        // up before the first attempt begins.
        if !has_attempts {
            transaction.commit().await?;
            return Ok(UsagePersistenceOutcome::Persisted);
        }

        let provider_id = event
            .provider_id
            .expect("validated attempted usage event has a provider ID");
        let upstream_model = event
            .upstream_model
            .as_deref()
            .expect("validated attempted usage event has an upstream model");
        sqlx::query(
            "INSERT INTO usage_request_anchors (request_id, request_started_at) \
             VALUES ($1, $2) ON CONFLICT DO NOTHING",
        )
        .bind(event.request_id)
        .bind(event.request_started_at)
        .execute(&mut *transaction)
        .await?;
        let pricing = sqlx::query(
            "SELECT selected.pricing_revision_id, selected.currency, \
                    selected.pricing_revision_id IS NOT NULL \
                      AND ($5::bigint IS NULL OR selected.input_per_million IS NOT NULL) \
                      AND ($6::bigint IS NULL OR selected.output_per_million IS NOT NULL) \
                      AND ($7::numeric IS NULL OR selected.unit_price IS NOT NULL) \
                      AS pricing_complete, \
                    CASE WHEN $8::boolean \
                               AND selected.pricing_revision_id IS NOT NULL \
                               AND ($5::bigint IS NULL OR selected.input_per_million IS NOT NULL) \
                               AND ($6::bigint IS NULL OR selected.output_per_million IS NOT NULL) \
                               AND ($7::numeric IS NULL OR selected.unit_price IS NOT NULL) \
                         THEN (COALESCE($5::numeric * selected.input_per_million / 1000000, 0) \
                             + COALESCE($6::numeric * selected.output_per_million / 1000000, 0) \
                             + COALESCE($7::numeric * selected.unit_price, 0))::text \
                         ELSE NULL END AS estimated_cost \
             FROM providers provider \
             LEFT JOIN LATERAL ( \
                 SELECT revision.id AS pricing_revision_id, price.input_per_million, \
                        price.output_per_million, price.unit_price, price.currency::text AS currency \
                 FROM pricing_revisions revision \
                 JOIN prices price ON price.pricing_revision_id = revision.id \
                 WHERE revision.effective_at <= $4 \
                   AND price.provider_kind = provider.kind \
                   AND (price.provider_id IS NULL OR price.provider_id = provider.id) \
                   AND price.model = $2 AND price.operation = $3 \
                 ORDER BY (price.provider_id IS NOT NULL) DESC, \
                          revision.effective_at DESC, revision.revision DESC LIMIT 1 \
             ) selected ON true \
             WHERE provider.id = $1",
        )
        .bind(provider_id)
        .bind(upstream_model)
        .bind(event.operation.as_str())
        .bind(event.observed_at)
        .bind(event.input_tokens)
        .bind(event.output_tokens)
        .bind(event.media_units)
        .bind(event.usage_complete)
        .fetch_one(&mut *transaction)
        .await?;
        let pricing_revision_id: Option<Uuid> = pricing.get("pricing_revision_id");
        let pricing_complete: bool = pricing.get("pricing_complete");
        let estimated_cost: Option<String> = pricing.get("estimated_cost");
        let currency = pricing
            .get::<Option<String>, _>("currency")
            .map(|value| value.trim().to_owned());
        let unpriced = !pricing_complete;
        sqlx::query(
            "INSERT INTO usage_facts \
             (id, request_id, request_started_at, api_key_id, provider_id, route_slug, upstream_model, operation, \
              surface, observed_at, input_tokens, output_tokens, cached_input_tokens, media_units, \
              estimated_cost, unpriced, usage_complete, pricing_revision_id, currency) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, \
                     $15::numeric, $16, $17, $18, $19) \
             ON CONFLICT (request_id) DO NOTHING",
        )
        .bind(event.event_id)
        .bind(event.request_id)
        .bind(event.request_started_at)
        .bind(event.api_key_id)
        .bind(provider_id)
        .bind(&event.route_slug)
        .bind(upstream_model)
        .bind(event.operation.as_str())
        .bind(event.surface.as_str())
        .bind(event.observed_at)
        .bind(event.input_tokens)
        .bind(event.output_tokens)
        .bind(event.cached_input_tokens)
        .bind(event.media_units)
        .bind(estimated_cost)
        .bind(unpriced)
        .bind(event.usage_complete)
        .bind(pricing_revision_id)
        .bind(currency)
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        Ok(UsagePersistenceOutcome::Persisted)
    }
}

fn usage_gap_count_from_decimal(value: Decimal) -> Result<u64, PersistenceError> {
    if !value.fract().is_zero() {
        return Err(PersistenceError::InvalidUsageGap);
    }
    u64::try_from(value).map_err(|_| PersistenceError::InvalidUsageGap)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn event() -> UsageEvent {
        let observed_at = Utc::now();
        let provider_id = Uuid::now_v7();
        UsageEvent {
            event_id: Uuid::now_v7(),
            request_id: Uuid::now_v7(),
            runtime_generation_id: Uuid::now_v7(),
            api_key_id: Uuid::now_v7(),
            provider_id: Some(provider_id),
            route_slug: "default".into(),
            upstream_model: Some("mock-model".into()),
            operation: OperationKind::Generation,
            surface: Surface::OpenAi,
            request_started_at: observed_at - chrono::Duration::milliseconds(10),
            request_completed_at: observed_at,
            observed_at,
            status_code: Some(200),
            error_class: None,
            committed: true,
            latency_ms: 10,
            first_byte_ms: Some(3),
            input_tokens: Some(1),
            output_tokens: Some(2),
            cached_input_tokens: None,
            media_units: None,
            usage_complete: true,
            unpriced: true,
            attempts: vec![UsageAttempt {
                id: Uuid::now_v7(),
                ordinal: 1,
                provider_id,
                upstream_model: "mock-model".into(),
                started_at: observed_at - chrono::Duration::milliseconds(10),
                completed_at: observed_at,
                status_code: Some(200),
                error_class: None,
                committed: true,
                latency_ms: 10,
                first_byte_ms: Some(3),
            }],
        }
    }

    #[test]
    fn overflow_is_counted_instead_of_silently_swallowed() {
        let (emitter, _receiver) = UsageEmitter::bounded(1);
        assert!(emitter.emit(event()).is_ok());
        assert!(emitter.emit(event()).is_err());
        let snapshot = emitter.snapshot();
        assert_eq!(snapshot.accepted, 1);
        assert_eq!(snapshot.persisted, 0);
        assert_eq!(snapshot.dropped, 1);
        assert_eq!(snapshot.abandoned, 0);
        assert!(snapshot.first_loss_at.is_some());
        assert!(snapshot.last_loss_at.is_some());
        assert!(!snapshot.complete());
    }

    #[tokio::test]
    async fn shutdown_accounts_for_every_accepted_but_unpersisted_event() {
        let (emitter, mut receiver) = UsageEmitter::bounded(2);
        emitter.emit(event()).unwrap();
        emitter.emit(event()).unwrap();

        receiver.record_abandoned(0).await;
        let snapshot = emitter.snapshot();
        assert_eq!(snapshot.accepted, 2);
        assert_eq!(snapshot.persisted, 0);
        assert_eq!(snapshot.dropped, 0);
        assert_eq!(snapshot.abandoned, 2);
        assert_eq!(snapshot.pending(), 0);
        assert_eq!(snapshot.lost(), 2);
        assert!(!snapshot.complete());
        assert!(matches!(emitter.emit(event()), Err(UsageEmitError::Closed)));
    }

    #[tokio::test]
    async fn concurrent_enqueue_and_shutdown_leave_no_unaccounted_reservation() {
        for _ in 0..128 {
            let (emitter, mut receiver) = UsageEmitter::bounded(1);
            let concurrent = emitter.clone();
            let barrier = Arc::new(tokio::sync::Barrier::new(2));
            let concurrent_barrier = Arc::clone(&barrier);
            let enqueue = tokio::spawn(async move {
                concurrent_barrier.wait().await;
                concurrent.emit(event())
            });
            barrier.wait().await;
            receiver.record_abandoned(0).await;
            let result = enqueue.await.unwrap();
            let snapshot = emitter.snapshot();
            assert_eq!(snapshot.accepted, snapshot.abandoned);
            assert_eq!(snapshot.pending(), 0);
            assert_eq!(snapshot.dropped, u64::from(result.is_err()));
        }
    }

    #[tokio::test]
    async fn shutdown_waits_for_an_outstanding_send_permit() {
        let (emitter, mut receiver) = UsageEmitter::bounded(1);
        let permit = emitter.sender.clone().try_reserve_owned().unwrap();
        emitter
            .health
            .accepted
            .fetch_add(1, std::sync::atomic::Ordering::SeqCst);

        let shutdown = tokio::spawn(async move {
            receiver.record_abandoned(0).await;
        });
        tokio::task::yield_now().await;
        assert!(!shutdown.is_finished());

        permit.send(event());
        shutdown.await.unwrap();
        let snapshot = emitter.snapshot();
        assert_eq!(snapshot.accepted, 1);
        assert_eq!(snapshot.abandoned, 1);
        assert_eq!(snapshot.pending(), 0);
    }

    #[test]
    fn snapshot_reconciles_the_send_before_acceptance_interval() {
        let health = UsageBufferHealth::default();
        health
            .persisted
            .store(1, std::sync::atomic::Ordering::SeqCst);
        let snapshot = health.snapshot();
        assert_eq!(snapshot.accepted, 1);
        assert_eq!(snapshot.persisted, 1);
        assert_eq!(snapshot.pending(), 0);
    }

    #[tokio::test]
    async fn invalid_valkey_configuration_accounts_for_queued_events() {
        let (emitter, receiver) = UsageEmitter::bounded(2);
        emitter.emit(event()).unwrap();
        emitter.emit(event()).unwrap();
        let (_shutdown_sender, shutdown) = watch::channel(false);

        assert!(
            receiver
                .run_connecting("://invalid", "usage", shutdown)
                .await
                .is_err()
        );
        let snapshot = emitter.snapshot();
        assert_eq!(snapshot.abandoned, 2);
        assert_eq!(snapshot.lost(), 2);
        assert!(!snapshot.complete());
    }

    #[test]
    fn retries_make_completeness_degraded_without_treating_backlog_as_loss() {
        let now = Utc::now();
        let snapshot = UsageBufferSnapshot {
            process_epoch: Uuid::now_v7(),
            started_at: now,
            accepted: 2,
            persisted: 1,
            dropped: 0,
            abandoned: 0,
            retrying: true,
            closed: false,
            first_loss_at: None,
            last_loss_at: None,
        };
        assert_eq!(snapshot.pending(), 1);
        assert_eq!(snapshot.lost(), 0);
        assert!(!snapshot.complete());
    }

    #[test]
    fn graceful_epoch_close_requires_writer_completion_and_full_accounting() {
        let now = Utc::now();
        let drained = UsageBufferSnapshot {
            process_epoch: Uuid::now_v7(),
            started_at: now,
            accepted: 2,
            persisted: 1,
            dropped: 0,
            abandoned: 1,
            retrying: false,
            closed: true,
            first_loss_at: Some(now),
            last_loss_at: Some(now),
        };
        assert!(drained.gracefully_drained());
        assert!(
            !UsageBufferSnapshot {
                closed: false,
                ..drained
            }
            .gracefully_drained()
        );
        assert!(
            !UsageBufferSnapshot {
                accepted: 3,
                ..drained
            }
            .gracefully_drained()
        );
    }

    #[test]
    fn durable_consumer_status_distinguishes_unknown_backlog_and_staleness() {
        let now = Utc::now();
        let unknown = UsageConsumerStatus::from_health(None, now);
        assert_eq!(unknown.state, UsageConsumerState::Unknown);
        assert!(!unknown.complete());

        let backlogged = UsageConsumerStatus::from_health(
            Some(UsageConsumerHealth {
                pending_events: 2,
                lag_events: 3,
                oldest_pending_at: Some(now - chrono::Duration::seconds(5)),
                checked_at: now,
            }),
            now,
        );
        assert_eq!(backlogged.state, UsageConsumerState::Backlogged);
        assert!(!backlogged.complete());

        let stale = UsageConsumerStatus::from_health(
            Some(UsageConsumerHealth {
                pending_events: 0,
                lag_events: 0,
                oldest_pending_at: None,
                checked_at: now - chrono::Duration::seconds(USAGE_CONSUMER_STALE_AFTER_SECONDS + 1),
            }),
            now,
        );
        assert_eq!(stale.state, UsageConsumerState::Stale);
        assert!(!stale.complete());

        let healthy = UsageConsumerStatus::from_health(
            Some(UsageConsumerHealth {
                pending_events: 0,
                lag_events: 0,
                oldest_pending_at: None,
                checked_at: now,
            }),
            now,
        );
        assert_eq!(healthy.state, UsageConsumerState::Healthy);
        assert!(healthy.complete());
    }

    #[test]
    fn serialized_event_has_no_content_fields() {
        let value = serde_json::to_value(event()).unwrap();
        for forbidden in [
            "prompt",
            "output",
            "reasoning",
            "headers",
            "credential",
            "tool_arguments",
        ] {
            assert!(value.get(forbidden).is_none());
        }
    }

    #[test]
    fn numeric_usage_gap_counts_are_integral_nonnegative_and_bounded() {
        assert_eq!(
            usage_gap_count_from_decimal(Decimal::from(2_u64)).unwrap(),
            2
        );
        assert_eq!(
            usage_gap_count_from_decimal(Decimal::from(u64::MAX)).unwrap(),
            u64::MAX
        );
        assert!(usage_gap_count_from_decimal(Decimal::NEGATIVE_ONE).is_err());
        assert!(usage_gap_count_from_decimal(Decimal::new(15, 1)).is_err());
        assert!(usage_gap_count_from_decimal(Decimal::from_parts(0, 0, 1, false, 0)).is_err());
    }
}
