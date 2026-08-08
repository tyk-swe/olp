use chrono::{Duration, Utc};
use olp_storage::{PersistenceError, request_metadata::RequestMetadataConsumerState};

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn request_metadata_consumer_backlog_is_durable_and_strictly_validated() {
    let db = olp_storage::test_support::TestDb::create_migrated("request_metadata_consumer_health")
        .await;
    let store = db.store(3).await;

    assert!(
        store
            .request_metadata_consumer_health()
            .await
            .unwrap()
            .is_none()
    );
    let oldest = Utc::now() - Duration::seconds(30);
    let recorded = store
        .report_request_metadata_consumer_health(2, 3, Some(oldest))
        .await
        .unwrap();
    assert_eq!(recorded.pending_events, 2);
    assert_eq!(recorded.lag_events, 3);
    assert_eq!(
        recorded.oldest_pending_at.unwrap().timestamp_micros(),
        oldest.timestamp_micros()
    );
    assert_eq!(
        store.request_metadata_consumer_health().await.unwrap(),
        Some(recorded)
    );
    let backlogged = store
        .request_metadata_consumer_status(Utc::now())
        .await
        .unwrap();
    assert_eq!(backlogged.state, RequestMetadataConsumerState::Backlogged);
    assert!(!backlogged.complete());

    assert!(matches!(
        store
            .report_request_metadata_consumer_health(0, 1, Some(oldest))
            .await,
        Err(PersistenceError::InvalidRequestMetadataGap)
    ));
    let drained = store
        .report_request_metadata_consumer_health(0, 0, None)
        .await
        .unwrap();
    assert_eq!(drained.pending_events, 0);
    assert!(drained.oldest_pending_at.is_none());
    let healthy = store
        .request_metadata_consumer_status(Utc::now())
        .await
        .unwrap();
    assert_eq!(healthy.state, RequestMetadataConsumerState::Healthy);
    assert!(healthy.complete());

    sqlx::query(
        "UPDATE request_metadata_consumer_health SET checked_at = now() - interval '1 minute'",
    )
    .execute(store.pool())
    .await
    .unwrap();
    let stale = store
        .request_metadata_consumer_status(Utc::now())
        .await
        .unwrap();
    assert_eq!(stale.state, RequestMetadataConsumerState::Stale);
    assert!(!stale.complete());
}

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn request_metadata_consumer_health_preserves_newer_sample() {
    let db =
        olp_storage::test_support::TestDb::create_migrated("request_metadata_health_sample").await;
    let store = db.store(3).await;
    let older_sampled_at = Utc::now() - Duration::seconds(10);
    let newer_sampled_at = older_sampled_at + Duration::seconds(1);
    let oldest_pending_at = older_sampled_at - Duration::seconds(30);

    let newer = store
        .report_request_metadata_consumer_health_sampled_at(0, 0, None, newer_sampled_at)
        .await
        .unwrap();
    assert_eq!(newer.pending_events, 0);
    assert_eq!(
        newer.checked_at.timestamp_micros(),
        newer_sampled_at.timestamp_micros()
    );

    let observed = store
        .report_request_metadata_consumer_health_sampled_at(
            7,
            0,
            Some(oldest_pending_at),
            older_sampled_at,
        )
        .await
        .unwrap();
    assert_eq!(observed, newer);
    assert_eq!(
        store.request_metadata_consumer_health().await.unwrap(),
        Some(newer)
    );
}
