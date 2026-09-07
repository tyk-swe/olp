use std::time::Duration as StdDuration;

use chrono::Duration;
use chrono::Utc;
use olp::database::error::Error;
use olp::usage::ingestion::delivery_health::ConsumerState;

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn request_metadata_consumer_backlog_is_durable_and_strictly_validated() {
    let db = olp::test_support::TestDb::create_migrated("request_metadata_consumer_health").await;
    let pool = db.pool(3).await;

    assert!(
        olp::usage::ingestion::delivery_health::request_metadata_consumer_health(&(pool),)
            .await
            .unwrap()
            .is_none()
    );
    let oldest = Utc::now() - Duration::seconds(30);
    let recorded = olp::usage::ingestion::delivery_health::report_request_metadata_consumer_health(
        &pool,
        2,
        3,
        Some(oldest),
    )
    .await
    .unwrap();
    assert_eq!(recorded.pending_events, 2);
    assert_eq!(recorded.lag_events, 3);
    assert_eq!(
        recorded.oldest_pending_at.unwrap().timestamp_micros(),
        oldest.timestamp_micros()
    );
    assert_eq!(
        olp::usage::ingestion::delivery_health::request_metadata_consumer_health(&(pool),)
            .await
            .unwrap(),
        Some(recorded)
    );
    let backlogged =
        olp::usage::ingestion::delivery_health::request_metadata_consumer_status(&pool, Utc::now())
            .await
            .unwrap();
    assert_eq!(backlogged.state, ConsumerState::Backlogged);
    assert!(!backlogged.complete());

    assert!(matches!(
        olp::usage::ingestion::delivery_health::report_request_metadata_consumer_health(
            &(pool),
            0,
            1,
            Some(oldest)
        )
        .await,
        Err(Error::InvalidRequestMetadataGap)
    ));
    for invalid in [
        olp::usage::ingestion::delivery_health::report_request_metadata_consumer_health(
            &pool, 1, 0, None,
        )
        .await,
        olp::usage::ingestion::delivery_health::report_request_metadata_consumer_health(
            &pool,
            u64::MAX,
            0,
            Some(oldest),
        )
        .await,
        olp::usage::ingestion::delivery_health::report_request_metadata_consumer_health(
            &pool,
            0,
            u64::MAX,
            None,
        )
        .await,
        olp::usage::ingestion::delivery_health::report_request_metadata_consumer_health(
            &pool,
            1,
            0,
            Some(Utc::now() + Duration::minutes(10)),
        )
        .await,
    ] {
        assert!(matches!(invalid, Err(Error::InvalidRequestMetadataGap)));
    }
    let drained = olp::usage::ingestion::delivery_health::report_request_metadata_consumer_health(
        &pool, 0, 0, None,
    )
    .await
    .unwrap();
    assert_eq!(drained.pending_events, 0);
    assert!(drained.oldest_pending_at.is_none());
    let healthy =
        olp::usage::ingestion::delivery_health::request_metadata_consumer_status(&pool, Utc::now())
            .await
            .unwrap();
    assert_eq!(healthy.state, ConsumerState::Healthy);
    assert!(healthy.complete());

    sqlx::query(
        "UPDATE request_metadata_consumer_health SET checked_at = now() - interval '1 minute'",
    )
    .execute(&pool)
    .await
    .unwrap();
    let stale =
        olp::usage::ingestion::delivery_health::request_metadata_consumer_status(&pool, Utc::now())
            .await
            .unwrap();
    assert_eq!(stale.state, ConsumerState::Stale);
    assert!(!stale.complete());
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn request_metadata_consumer_health_preserves_newer_sample() {
    let db = olp::test_support::TestDb::create_migrated("request_metadata_health_sample").await;
    let pool = db.pool(3).await;
    let older_sampled_at = Utc::now() - Duration::seconds(10);
    let newer_sampled_at = older_sampled_at + Duration::seconds(1);
    let oldest_pending_at = older_sampled_at - Duration::seconds(30);

    let newer =
        olp::usage::ingestion::delivery_health::report_request_metadata_consumer_health_sampled_at(
            &pool,
            0,
            0,
            None,
            newer_sampled_at,
        )
        .await
        .unwrap();
    assert_eq!(newer.pending_events, 0);
    assert_eq!(
        newer.checked_at.timestamp_micros(),
        newer_sampled_at.timestamp_micros()
    );

    let observed =
        olp::usage::ingestion::delivery_health::report_request_metadata_consumer_health_sampled_at(
            &pool,
            7,
            0,
            Some(oldest_pending_at),
            older_sampled_at,
        )
        .await
        .unwrap();
    assert_eq!(observed, newer);
    assert_eq!(
        olp::usage::ingestion::delivery_health::request_metadata_consumer_health(&(pool),)
            .await
            .unwrap(),
        Some(newer)
    );

    let latest_sampled_at = newer_sampled_at + Duration::seconds(1);
    let latest_oldest_pending_at = latest_sampled_at - Duration::seconds(30);
    let latest =
        olp::usage::ingestion::delivery_health::report_request_metadata_consumer_health_sampled_at(
            &pool,
            4,
            2,
            Some(latest_oldest_pending_at),
            latest_sampled_at,
        )
        .await
        .unwrap();
    assert_eq!(latest.pending_events, 4);
    assert_eq!(latest.lag_events, 2);
    assert_eq!(
        latest.checked_at.timestamp_micros(),
        latest_sampled_at.timestamp_micros()
    );
    assert_eq!(
        olp::usage::ingestion::delivery_health::request_metadata_consumer_health(&(pool),)
            .await
            .unwrap(),
        Some(latest)
    );
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn future_skewed_consumer_sample_does_not_block_current_sample() {
    let db = olp::test_support::TestDb::create_migrated("request_metadata_future_sample").await;
    let pool = db.pool(3).await;

    let future_sampled_at = Utc::now() + Duration::hours(1);
    let future =
        olp::usage::ingestion::delivery_health::report_request_metadata_consumer_health_sampled_at(
            &pool,
            0,
            0,
            None,
            future_sampled_at,
        )
        .await
        .unwrap();
    assert!(future.checked_at < future_sampled_at);

    tokio::time::sleep(StdDuration::from_millis(10)).await;
    let current_sampled_at = Utc::now();
    let current_oldest_pending_at = current_sampled_at - Duration::seconds(30);
    let current =
        olp::usage::ingestion::delivery_health::report_request_metadata_consumer_health_sampled_at(
            &pool,
            5,
            1,
            Some(current_oldest_pending_at),
            current_sampled_at,
        )
        .await
        .unwrap();
    assert_eq!(current.pending_events, 5);
    assert_eq!(current.lag_events, 1);
    assert_eq!(
        olp::usage::ingestion::delivery_health::request_metadata_consumer_health(&(pool),)
            .await
            .unwrap(),
        Some(current)
    );
}
