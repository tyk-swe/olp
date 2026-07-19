use chrono::{Duration, Utc};
use olp_storage::{PersistenceError, PgStore, UsageConsumerState};

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn usage_consumer_backlog_is_durable_and_strictly_validated() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 3).await.unwrap();
    store.migrate().await.unwrap();

    assert!(store.usage_consumer_health().await.unwrap().is_none());
    let oldest = Utc::now() - Duration::seconds(30);
    let recorded = store
        .report_usage_consumer_health(2, 3, Some(oldest))
        .await
        .unwrap();
    assert_eq!(recorded.pending_events, 2);
    assert_eq!(recorded.lag_events, 3);
    assert_eq!(
        recorded.oldest_pending_at.unwrap().timestamp_micros(),
        oldest.timestamp_micros()
    );
    assert_eq!(store.usage_consumer_health().await.unwrap(), Some(recorded));
    let backlogged = store.usage_consumer_status(Utc::now()).await.unwrap();
    assert_eq!(backlogged.state, UsageConsumerState::Backlogged);
    assert!(!backlogged.complete());

    assert!(matches!(
        store.report_usage_consumer_health(0, 1, Some(oldest)).await,
        Err(PersistenceError::InvalidUsageGap)
    ));
    let drained = store
        .report_usage_consumer_health(0, 0, None)
        .await
        .unwrap();
    assert_eq!(drained.pending_events, 0);
    assert!(drained.oldest_pending_at.is_none());
    let healthy = store.usage_consumer_status(Utc::now()).await.unwrap();
    assert_eq!(healthy.state, UsageConsumerState::Healthy);
    assert!(healthy.complete());

    sqlx::query("UPDATE usage_consumer_health SET checked_at = now() - interval '1 minute'")
        .execute(store.pool())
        .await
        .unwrap();
    let stale = store.usage_consumer_status(Utc::now()).await.unwrap();
    assert_eq!(stale.state, UsageConsumerState::Stale);
    assert!(!stale.complete());
}
