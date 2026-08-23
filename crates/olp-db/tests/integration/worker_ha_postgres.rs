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
async fn migration_rebuilds_an_existing_unrecorded_attempt_usage_event_index() {
    let db = TestDb::create_empty("migration_retry").await;
    let store = db.store(2).await;
    store.migrate_to(33).await.unwrap();
    sqlx::query(
        "CREATE INDEX CONCURRENTLY attempt_usage_facts_event_id_idx \
         ON attempt_usage_facts(event_id)",
    )
    .execute(store.pool())
    .await
    .unwrap();

    store.migrate().await.unwrap();

    let (valid, applied): (bool, bool) = sqlx::query_as(
        "SELECT index.indisvalid, EXISTS ( \
           SELECT 1 FROM _sqlx_migrations WHERE version = 34 AND success \
         ) \
         FROM pg_index index \
         WHERE index.indexrelid = 'attempt_usage_facts_event_id_idx'::regclass",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert!(valid);
    assert!(applied);
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make db-test"]
async fn maintenance_discards_a_session_that_does_not_acquire_the_lock() {
    let db = TestDb::create_migrated("maintenance_lock_session").await;
    let contender = db.store(1).await;
    let leader = db.store(1).await;
    let contender_pid: i32 = sqlx::query_scalar("SELECT pg_backend_pid()")
        .fetch_one(contender.pool())
        .await
        .unwrap();
    let mut leader_connection = leader.pool().acquire().await.unwrap();
    let acquired: bool = sqlx::query_scalar("SELECT pg_try_advisory_lock($1)")
        .bind(MAINTENANCE_LOCK_ID)
        .fetch_one(&mut *leader_connection)
        .await
        .unwrap();
    assert!(acquired);

    let report = contender.run_maintenance(Utc::now()).await.unwrap();

    assert!(!report.lock_acquired);
    let replacement_pid: i32 = sqlx::query_scalar("SELECT pg_backend_pid()")
        .fetch_one(contender.pool())
        .await
        .unwrap();
    assert_ne!(replacement_pid, contender_pid);
    let released: bool = sqlx::query_scalar("SELECT pg_advisory_unlock($1)")
        .bind(MAINTENANCE_LOCK_ID)
        .fetch_one(&mut *leader_connection)
        .await
        .unwrap();
    assert!(released);
}

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
async fn maintenance_repeats_committed_retention_batches() {
    let db = TestDb::create_migrated("maintenance_batches").await;
    let store = db.store(2).await;
    let expired_rows = 50_001_i64;
    sqlx::query("CREATE TABLE maintenance_batch_transactions (transaction_id xid8 PRIMARY KEY)")
        .execute(store.pool())
        .await
        .unwrap();
    sqlx::query(
        "CREATE FUNCTION record_maintenance_batch_transaction() RETURNS trigger AS $$ \
         BEGIN \
           INSERT INTO maintenance_batch_transactions VALUES (pg_current_xact_id()) \
           ON CONFLICT DO NOTHING; \
           RETURN NULL; \
         END; \
         $$ LANGUAGE plpgsql",
    )
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "CREATE TRIGGER record_maintenance_batch_transaction \
         AFTER DELETE ON audit_events FOR EACH STATEMENT \
         EXECUTE FUNCTION record_maintenance_batch_transaction()",
    )
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO audit_events (id, action, resource_type, outcome, occurred_at) \
         SELECT uuidv7(), 'batch-test', 'batch-test', 'succeeded', \
                now() - interval '366 days' \
         FROM generate_series(1, $1)",
    )
    .bind(expired_rows)
    .execute(store.pool())
    .await
    .unwrap();

    let report = store.run_maintenance(Utc::now()).await.unwrap();

    assert!(report.lock_acquired);
    assert_eq!(report.audit_rows, expired_rows as u64);
    let (retained, transactions): (i64, i64) = sqlx::query_as(
        "SELECT (SELECT count(*) FROM audit_events), \
                (SELECT count(*) FROM maintenance_batch_transactions)",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(retained, 0);
    assert_eq!(transactions, 2);
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make db-test"]
async fn three_maintenance_replicas_never_overlap_a_destructive_pass() {
    let db = TestDb::create_migrated("maintenance_three_replicas").await;
    let store = db.store(12).await;

    // Keep the elected pass blocked immediately after it acquires the
    // session-level advisory lock. The other two replicas must skip
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
            let held: bool = sqlx::query_scalar(
                "SELECT EXISTS ( \
                   SELECT 1 FROM pg_locks \
                   WHERE locktype = 'advisory' AND granted \
                     AND database = ( \
                       SELECT oid FROM pg_database WHERE datname = current_database() \
                     ) \
                     AND ((classid::bigint << 32) | objid::bigint) = $1 \
                     AND objsubid = 1 \
                 )",
            )
            .bind(MAINTENANCE_LOCK_ID)
            .fetch_one(store.pool())
            .await
            .unwrap();
            if held {
                return;
            }
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
