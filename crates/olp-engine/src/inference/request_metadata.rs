//! Content-free request accounting events and their bounded local handoff.
//!
//! The receiver exposes only the bookkeeping operations needed by a durable
//! delivery adapter. Infrastructure details such as serialization, retries,
//! and the backing stream remain outside the engine.

use std::sync::{
    Arc,
    atomic::{AtomicBool, AtomicI64, AtomicU64, Ordering},
};

use chrono::{DateTime, Utc};
use rust_decimal::Decimal;
use serde::{Deserialize, Serialize};
use thiserror::Error;
use tokio::sync::mpsc;
use uuid::Uuid;

use crate::domain::{OperationKind, Surface};

/// Metadata-only request envelope. Content-bearing fields do not exist in this
/// type, making accidental prompt/output persistence structurally impossible.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RequestMetadataEvent {
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
    pub attempts: Vec<RequestAttemptMetadata>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RequestAttemptMetadata {
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
    /// Attempt-local usage and billing evidence. This is optional only for
    /// events serialized by pre-0033 binaries; new producers always populate
    /// it so persistence never has to attach request-level usage to the last
    /// provider by convention.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub usage: Option<RequestAttemptUsageMetadata>,
}

/// Content-free usage evidence captured for one provider attempt. Pricing is
/// deliberately resolved only while persisting the event, against the
/// immutable pricing revision effective at `observed_at`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RequestAttemptUsageMetadata {
    pub observed: bool,
    pub complete: bool,
    pub billing_uncertain: bool,
    pub input_tokens: Option<i64>,
    pub output_tokens: Option<i64>,
    pub cached_input_tokens: Option<i64>,
    pub media_units: Option<Decimal>,
}

#[derive(Clone)]
pub struct RequestMetadataEmitter {
    sender: mpsc::Sender<RequestMetadataEvent>,
    health: Arc<RequestMetadataBufferHealth>,
}

impl RequestMetadataEmitter {
    #[must_use]
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
                self.health.accepted.fetch_add(1, Ordering::SeqCst);
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

    #[must_use]
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

/// The engine side of the bounded handoff to a durable delivery adapter.
///
/// Bookkeeping stays encapsulated here so adapters cannot mutate counters or
/// atomics directly.
pub struct RequestMetadataReceiver {
    receiver: mpsc::Receiver<RequestMetadataEvent>,
    health: Arc<RequestMetadataBufferHealth>,
}

impl RequestMetadataReceiver {
    /// Receives one buffered event without exposing prompts or response bodies,
    /// which are absent from [`RequestMetadataEvent`].
    pub async fn recv_next(&mut self) -> Option<RequestMetadataEvent> {
        self.receiver.recv().await
    }

    /// Records one event after its durable adapter has accepted it.
    pub fn mark_persisted(&self) {
        self.health.persisted.fetch_add(1, Ordering::SeqCst);
    }

    /// Reflects whether the durable adapter is currently retrying delivery.
    pub fn set_retrying(&self, retrying: bool) {
        self.health.retrying.store(retrying, Ordering::Relaxed);
    }

    /// Stops new reservations and drains until outstanding permits are either
    /// published or released, ensuring every accepted event is accounted for.
    /// `current_event_count` accounts for an event already removed by the
    /// adapter but not durably persisted.
    pub async fn abandon_and_drain(&mut self, current_event_count: u64) {
        self.receiver.close();
        let mut abandoned = current_event_count;
        while self.receiver.recv().await.is_some() {
            abandoned = abandoned.saturating_add(1);
        }
        self.health.record_abandoned(abandoned);
    }
}

struct RequestMetadataBufferHealth {
    process_epoch: Uuid,
    started_at_ms: i64,
    accepted: AtomicU64,
    persisted: AtomicU64,
    dropped: AtomicU64,
    abandoned: AtomicU64,
    retrying: AtomicBool,
    first_loss_at_ms: AtomicI64,
    last_loss_at_ms: AtomicI64,
}

impl Default for RequestMetadataBufferHealth {
    fn default() -> Self {
        Self {
            process_epoch: Uuid::now_v7(),
            started_at_ms: Utc::now().timestamp_millis(),
            accepted: AtomicU64::new(0),
            persisted: AtomicU64::new(0),
            dropped: AtomicU64::new(0),
            abandoned: AtomicU64::new(0),
            retrying: AtomicBool::new(false),
            first_loss_at_ms: AtomicI64::new(0),
            last_loss_at_ms: AtomicI64::new(0),
        }
    }
}

impl RequestMetadataBufferHealth {
    fn record_dropped(&self, count: u64) {
        self.dropped.fetch_add(count, Ordering::Relaxed);
        self.record_loss_time(count);
    }

    fn record_abandoned(&self, count: u64) {
        self.abandoned.fetch_add(count, Ordering::SeqCst);
        self.record_loss_time(count);
    }

    fn record_loss_time(&self, count: u64) {
        if count == 0 {
            return;
        }
        let now = Utc::now().timestamp_millis();
        let _ =
            self.first_loss_at_ms
                .compare_exchange(0, now, Ordering::Relaxed, Ordering::Relaxed);
        self.last_loss_at_ms.store(now, Ordering::Relaxed);
    }

    fn snapshot(&self) -> RequestMetadataBufferSnapshot {
        // Downstream counts can never precede acceptance, but retain the lower
        // bound as a fail-closed guard against impossible durable checkpoints.
        let persisted = self.persisted.load(Ordering::SeqCst);
        let abandoned = self.abandoned.load(Ordering::SeqCst);
        let accepted = self
            .accepted
            .load(Ordering::SeqCst)
            .max(persisted.saturating_add(abandoned));
        RequestMetadataBufferSnapshot {
            process_epoch: self.process_epoch,
            started_at: timestamp_millis(self.started_at_ms).unwrap_or_else(Utc::now),
            accepted,
            persisted,
            dropped: self.dropped.load(Ordering::Relaxed),
            abandoned,
            retrying: self.retrying.load(Ordering::Relaxed),
            closed: false,
            first_loss_at: timestamp_millis(self.first_loss_at_ms.load(Ordering::Relaxed)),
            last_loss_at: timestamp_millis(self.last_loss_at_ms.load(Ordering::Relaxed)),
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
    #[must_use]
    pub fn complete(&self) -> bool {
        self.dropped == 0 && self.abandoned == 0 && !self.retrying && !self.closed
    }

    #[must_use]
    pub fn pending(&self) -> u64 {
        self.accepted
            .saturating_sub(self.persisted.saturating_add(self.abandoned))
    }

    #[must_use]
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

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use super::*;

    fn event() -> RequestMetadataEvent {
        let observed_at = Utc::now();
        let provider_id = Uuid::now_v7();
        RequestMetadataEvent {
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
            attempts: vec![RequestAttemptMetadata {
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
                usage: None,
            }],
        }
    }

    #[test]
    fn overflow_is_counted_instead_of_silently_swallowed() {
        let (emitter, _receiver) = RequestMetadataEmitter::bounded(1);
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
        let (emitter, mut receiver) = RequestMetadataEmitter::bounded(2);
        emitter.emit(event()).unwrap();
        emitter.emit(event()).unwrap();

        receiver.abandon_and_drain(0).await;
        let snapshot = emitter.snapshot();
        assert_eq!(snapshot.accepted, 2);
        assert_eq!(snapshot.persisted, 0);
        assert_eq!(snapshot.dropped, 0);
        assert_eq!(snapshot.abandoned, 2);
        assert_eq!(snapshot.pending(), 0);
        assert_eq!(snapshot.lost(), 2);
        assert!(!snapshot.complete());
        assert!(matches!(
            emitter.emit(event()),
            Err(RequestMetadataEmitError::Closed)
        ));
    }

    #[tokio::test]
    async fn concurrent_enqueue_and_shutdown_leave_no_unaccounted_reservation() {
        for _ in 0..128 {
            let (emitter, mut receiver) = RequestMetadataEmitter::bounded(1);
            let concurrent = emitter.clone();
            let barrier = Arc::new(tokio::sync::Barrier::new(2));
            let concurrent_barrier = Arc::clone(&barrier);
            let enqueue = tokio::spawn(async move {
                concurrent_barrier.wait().await;
                concurrent.emit(event())
            });
            barrier.wait().await;
            receiver.abandon_and_drain(0).await;
            let result = enqueue.await.unwrap();
            let snapshot = emitter.snapshot();
            assert_eq!(snapshot.accepted, snapshot.abandoned);
            assert_eq!(snapshot.pending(), 0);
            assert_eq!(snapshot.dropped, u64::from(result.is_err()));
        }
    }

    #[tokio::test]
    async fn shutdown_waits_for_an_outstanding_send_permit() {
        let (emitter, mut receiver) = RequestMetadataEmitter::bounded(1);
        let permit = emitter.sender.clone().try_reserve_owned().unwrap();
        emitter.health.accepted.fetch_add(1, Ordering::SeqCst);

        let shutdown = tokio::spawn(async move {
            receiver.abandon_and_drain(0).await;
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

    #[tokio::test]
    async fn adapter_updates_health_without_exposing_counters() {
        let (emitter, mut receiver) = RequestMetadataEmitter::bounded(1);
        emitter.emit(event()).unwrap();
        assert!(receiver.recv_next().await.is_some());
        receiver.set_retrying(true);
        assert!(emitter.snapshot().retrying);
        receiver.mark_persisted();
        receiver.set_retrying(false);

        let snapshot = emitter.snapshot();
        assert_eq!(snapshot.accepted, 1);
        assert_eq!(snapshot.persisted, 1);
        assert_eq!(snapshot.pending(), 0);
        assert!(!snapshot.retrying);
    }

    #[test]
    fn retries_make_completeness_degraded_without_treating_backlog_as_loss() {
        let now = Utc::now();
        let snapshot = RequestMetadataBufferSnapshot {
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
        let drained = RequestMetadataBufferSnapshot {
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
            !RequestMetadataBufferSnapshot {
                closed: false,
                ..drained
            }
            .gracefully_drained()
        );
        assert!(
            !RequestMetadataBufferSnapshot {
                accepted: 3,
                ..drained
            }
            .gracefully_drained()
        );
    }

    #[test]
    fn serialized_event_has_no_content_fields() {
        let value = serde_json::to_value(event()).unwrap();
        assert_no_content_fields(&value);
    }

    fn assert_no_content_fields(value: &serde_json::Value) {
        for forbidden in [
            "prompt",
            "response",
            "output",
            "reasoning",
            "headers",
            "credential",
            "tool_arguments",
            "uploaded_media",
            "provider_body",
        ] {
            assert!(value.get(forbidden).is_none());
        }
        match value {
            serde_json::Value::Array(values) => values.iter().for_each(assert_no_content_fields),
            serde_json::Value::Object(values) => values.values().for_each(assert_no_content_fields),
            _ => {}
        }
    }

    #[test]
    fn event_serialized_before_attempt_usage_deserializes_safely() {
        let value = serde_json::to_value(event()).unwrap();
        assert!(value["attempts"][0].get("usage").is_none());
        let decoded: RequestMetadataEvent = serde_json::from_value(value).unwrap();
        assert_eq!(decoded.attempts.len(), 1);
        assert!(decoded.attempts[0].usage.is_none());
    }
}
