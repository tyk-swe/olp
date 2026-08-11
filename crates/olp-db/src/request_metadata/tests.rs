use super::*;

#[test]
fn durable_consumer_status_distinguishes_unknown_backlog_and_staleness() {
    let now = Utc::now();
    let unknown = RequestMetadataConsumerStatus::from_health(None, now);
    assert_eq!(unknown.state, RequestMetadataConsumerState::Unknown);
    assert!(!unknown.complete());

    let backlogged = RequestMetadataConsumerStatus::from_health(
        Some(RequestMetadataConsumerHealth {
            pending_events: 2,
            lag_events: 3,
            oldest_pending_at: Some(now - chrono::Duration::seconds(5)),
            checked_at: now,
        }),
        now,
    );
    assert_eq!(backlogged.state, RequestMetadataConsumerState::Backlogged);
    assert!(!backlogged.complete());

    let stale = RequestMetadataConsumerStatus::from_health(
        Some(RequestMetadataConsumerHealth {
            pending_events: 0,
            lag_events: 0,
            oldest_pending_at: None,
            checked_at: now
                - chrono::Duration::seconds(REQUEST_METADATA_CONSUMER_STALE_AFTER_SECONDS + 1),
        }),
        now,
    );
    assert_eq!(stale.state, RequestMetadataConsumerState::Stale);
    assert!(!stale.complete());

    let healthy = RequestMetadataConsumerStatus::from_health(
        Some(RequestMetadataConsumerHealth {
            pending_events: 0,
            lag_events: 0,
            oldest_pending_at: None,
            checked_at: now,
        }),
        now,
    );
    assert_eq!(healthy.state, RequestMetadataConsumerState::Healthy);
    assert!(healthy.complete());
}

#[test]
fn numeric_request_metadata_gap_counts_are_integral_nonnegative_and_bounded() {
    assert_eq!(
        request_metadata_gap_count_from_decimal(Decimal::from(2_u64)).unwrap(),
        2
    );
    assert_eq!(
        request_metadata_gap_count_from_decimal(Decimal::from(u64::MAX)).unwrap(),
        u64::MAX
    );
    assert!(request_metadata_gap_count_from_decimal(Decimal::NEGATIVE_ONE).is_err());
    assert!(request_metadata_gap_count_from_decimal(Decimal::new(15, 1)).is_err());
    assert!(
        request_metadata_gap_count_from_decimal(Decimal::from_parts(0, 0, 1, false, 0)).is_err()
    );
}
