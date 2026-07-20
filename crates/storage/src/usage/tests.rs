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
