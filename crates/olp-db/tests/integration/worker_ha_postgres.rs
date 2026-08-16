use std::{sync::Arc, time::Duration as StdDuration};

use chrono::{Duration, Utc};
use olp_db::{
    maintenance::MAINTENANCE_LOCK_ID,
    request_metadata::delivery_health::ConsumerState,
    test_support::TestDb,
    worker_health::{RequestMetadataConsumerActivity, WorkerTask, WorkerTaskState},
};
use tokio::sync::Barrier;
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires PostgreSQL via make db-test"]
async fn three_workers_add_recovery_counters_monotonically_and_stale_as_a_fleet() {
    let db = TestDb::create_migrated("worker_health_three_replicas").await;
    let store = db.store(12).await;
    let barrier = Arc::new(Barrier::new(4));
    let mut workers = tokio::task::JoinSet::new();
    for _ in 0..3 {
        let store = store.clone();
        let barrier = Arc::clone(&barrier);
        workers.spawn(async move {
            barrier.wait().await;
            store
                .report_request_metadata_consumer_health(0, 0, None)
                .await
                .unwrap();
            store
                .report_request_metadata_consumer_activity(RequestMetadataConsumerActivity {
                    reclaimed: 1,
                    recovered: 1,
                    duplicates: 1,
                    processed: 1,
                })
                .await
                .unwrap();
        });
    }
    barrier.wait().await;
    while let Some(result) = workers.join_next().await {
        result.unwrap();
    }

    let counters = store.worker_recovery_counters().await.unwrap();
    assert_eq!(counters.request_metadata_reclaimed, 3);
    assert_eq!(counters.request_metadata_recovered, 3);
    assert_eq!(counters.request_metadata_duplicates, 3);
    assert_eq!(counters.request_metadata_processed, 3);

    let consumer = store
        .request_metadata_consumer_health()
        .await
        .unwrap()
        .unwrap();
    let current = store.worker_task_health(consumer.checked_at).await.unwrap();
    let metadata = current
        .tasks
        .iter()
        .find(|task| task.task == WorkerTask::RequestMetadataConsumer)
        .unwrap();
    assert_eq!(metadata.state, WorkerTaskState::Healthy);
    assert_eq!(metadata.successes_total, 6);

    let stale_at = consumer.checked_at + Duration::seconds(22);
    let stale_consumer = store
        .request_metadata_consumer_status(stale_at)
        .await
        .unwrap();
    assert_eq!(stale_consumer.state, ConsumerState::Stale);
    let stale = store.worker_task_health(stale_at).await.unwrap();
    assert_eq!(
        stale
            .tasks
            .iter()
            .find(|task| task.task == WorkerTask::RequestMetadataConsumer)
            .unwrap()
            .state,
        WorkerTaskState::Stale
    );
    assert_eq!(store.worker_recovery_counters().await.unwrap(), counters);
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make db-test"]
async fn three_maintenance_replicas_never_overlap_a_destructive_pass() {
    let db = TestDb::create_migrated("maintenance_three_replicas").await;
    let store = db.store(12).await;

    // Keep the elected pass blocked immediately after it acquires the
    // transaction-level advisory lock. The other two replicas must skip
    // instead of queuing a second destructive transaction.
    let mut table_blocker = store.pool().begin().await.unwrap();
    sqlx::query("LOCK TABLE settings IN ACCESS EXCLUSIVE MODE")
        .execute(&mut *table_blocker)
        .await
        .unwrap();
    let first_store = store.clone();
    let first = tokio::spawn(async move { first_store.run_maintenance(Utc::now()).await.unwrap() });

    tokio::time::timeout(StdDuration::from_secs(5), async {
        loop {
            let mut connection = store.pool().acquire().await.unwrap();
            let acquired: bool = sqlx::query_scalar("SELECT pg_try_advisory_lock($1)")
                .bind(MAINTENANCE_LOCK_ID)
                .fetch_one(&mut *connection)
                .await
                .unwrap();
            if !acquired {
                return;
            }
            let released: bool = sqlx::query_scalar("SELECT pg_advisory_unlock($1)")
                .bind(MAINTENANCE_LOCK_ID)
                .fetch_one(&mut *connection)
                .await
                .unwrap();
            assert!(released);
            tokio::time::sleep(StdDuration::from_millis(10)).await;
        }
    })
    .await
    .expect("the first maintenance replica never acquired leadership");

    let second_store = store.clone();
    let third_store = store.clone();
    let second =
        tokio::spawn(async move { second_store.run_maintenance(Utc::now()).await.unwrap() });
    let third = tokio::spawn(async move { third_store.run_maintenance(Utc::now()).await.unwrap() });
    let (second, third) = tokio::time::timeout(StdDuration::from_secs(5), async {
        tokio::join!(second, third)
    })
    .await
    .expect("contending maintenance replicas waited instead of skipping");
    assert!(!second.unwrap().lock_acquired);
    assert!(!third.unwrap().lock_acquired);

    table_blocker.rollback().await.unwrap();
    assert!(first.await.unwrap().lock_acquired);
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make db-test"]
async fn three_epoch_detectors_record_each_stale_epoch_once() {
    let db = TestDb::create_migrated("epoch_detection_three_replicas").await;
    let store = db.store(12).await;
    let now = Utc::now();
    let epoch = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO request_metadata_gateway_epochs \
         (gateway_instance, process_epoch, started_at, accepted, persisted, dropped, abandoned, \
          retrying, writer_closed, updated_at) \
         VALUES ('three-replica-gateway', $1, $2, 5, 2, 0, 0, false, false, $2)",
    )
    .bind(epoch)
    .bind(now - Duration::minutes(2))
    .execute(store.pool())
    .await
    .unwrap();

    let candidates = run_three_epoch_detectors(&store, now).await;
    assert_eq!(
        candidates
            .iter()
            .map(|report| report.candidate_epochs)
            .sum::<u64>(),
        1
    );
    assert_eq!(
        candidates
            .iter()
            .map(|report| report.detected_epochs)
            .sum::<u64>(),
        0
    );

    let detections = run_three_epoch_detectors(&store, now + Duration::seconds(11)).await;
    assert_eq!(
        detections
            .iter()
            .map(|report| report.detected_epochs)
            .sum::<u64>(),
        1
    );
    assert_eq!(
        detections
            .iter()
            .map(|report| report.uncertain_event_lower_bound)
            .sum::<u64>(),
        3
    );
    let gap_count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM request_metadata_ingestion_gaps \
         WHERE gateway_instance = 'three-replica-gateway' \
           AND reason = 'gateway_epoch_unclean_shutdown'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(gap_count, 1);
}

async fn run_three_epoch_detectors(
    store: &olp_db::store::Store,
    now: chrono::DateTime<Utc>,
) -> Vec<olp_db::request_metadata::reconciliation::EpochDetection> {
    let barrier = Arc::new(Barrier::new(4));
    let mut workers = tokio::task::JoinSet::new();
    for _ in 0..3 {
        let store = store.clone();
        let barrier = Arc::clone(&barrier);
        workers.spawn(async move {
            barrier.wait().await;
            store
                .detect_stale_request_metadata_gateway_epochs(now)
                .await
                .unwrap()
        });
    }
    barrier.wait().await;
    let mut reports = Vec::new();
    while let Some(result) = workers.join_next().await {
        reports.push(result.unwrap());
    }
    reports
}
