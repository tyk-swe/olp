use std::sync::Arc;
use std::time::Duration as StdDuration;

use chrono::Duration;
use chrono::Utc;
use olp::observability::workers::RequestMetadataConsumerActivity;
use olp::observability::workers::WorkerTask;
use olp::observability::workers::WorkerTaskState;
use olp::test_support::TestDb;
use olp::usage::ingestion::delivery_health::ConsumerState;
use olp::usage::retention::MAINTENANCE_LOCK_ID;
use tokio::sync::Barrier;
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires PostgreSQL via make integration"]
async fn maintenance_discards_a_session_that_does_not_acquire_the_lock() {
    let db = TestDb::create_migrated("maintenance_lock_session").await;
    let contender = db.pool(1).await;
    let leader = db.pool(1).await;
    let contender_pid: i32 = sqlx::query_scalar("SELECT pg_backend_pid()")
        .fetch_one(&contender)
        .await
        .unwrap();
    let mut leader_connection = leader.acquire().await.unwrap();
    let acquired: bool = sqlx::query_scalar("SELECT pg_try_advisory_lock($1)")
        .bind(MAINTENANCE_LOCK_ID)
        .fetch_one(&mut *leader_connection)
        .await
        .unwrap();
    assert!(acquired);

    let report = olp::usage::retention::run_maintenance(&contender, Utc::now())
        .await
        .unwrap();

    assert!(!report.lock_acquired);
    let replacement_pid: i32 = sqlx::query_scalar("SELECT pg_backend_pid()")
        .fetch_one(&contender)
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
#[ignore = "requires PostgreSQL via make integration"]
async fn three_workers_add_recovery_counters_monotonically_and_stale_as_a_fleet() {
    let db = TestDb::create_migrated("worker_health_three_replicas").await;
    let pool = db.pool(12).await;
    let barrier = Arc::new(Barrier::new(4));
    let mut workers = tokio::task::JoinSet::new();
    for _ in 0..3 {
        let pool = pool.clone();
        let barrier = Arc::clone(&barrier);
        workers.spawn(async move {
            barrier.wait().await;
            olp::usage::ingestion::delivery_health::report_request_metadata_consumer_health(
                &pool, 0, 0, None,
            )
            .await
            .unwrap();
            olp::observability::workers::report_request_metadata_consumer_activity(
                &pool,
                RequestMetadataConsumerActivity {
                    reclaimed: 1,
                    recovered: 1,
                    duplicates: 1,
                    processed: 1,
                },
            )
            .await
            .unwrap();
        });
    }
    barrier.wait().await;
    while let Some(result) = workers.join_next().await {
        result.unwrap();
    }

    let counters = olp::observability::workers::worker_recovery_counters(&pool)
        .await
        .unwrap();
    assert_eq!(counters.request_metadata_reclaimed, 3);
    assert_eq!(counters.request_metadata_recovered, 3);
    assert_eq!(counters.request_metadata_duplicates, 3);
    assert_eq!(counters.request_metadata_processed, 3);

    let consumer = olp::usage::ingestion::delivery_health::request_metadata_consumer_health(&pool)
        .await
        .unwrap()
        .unwrap();
    let current = olp::observability::workers::worker_task_health(&pool)
        .await
        .unwrap();
    let metadata = current
        .tasks
        .iter()
        .find(|task| task.task == WorkerTask::RequestMetadataConsumer)
        .unwrap();
    assert_eq!(metadata.state, WorkerTaskState::Healthy);
    assert_eq!(metadata.successes_total, 6);

    let stale_at = consumer.checked_at + Duration::seconds(22);
    let stale_consumer =
        olp::usage::ingestion::delivery_health::request_metadata_consumer_status(&pool, stale_at)
            .await
            .unwrap();
    assert_eq!(stale_consumer.state, ConsumerState::Stale);
    // Task ages come from the database clock, so age the checkpoint itself
    // rather than handing the reader a future wall-clock reading.
    sqlx::query(
        "UPDATE worker_task_health \
         SET checked_at = checked_at - interval '22 seconds', \
             last_success_at = last_success_at - interval '22 seconds' \
         WHERE task = 'request_metadata_consumer'",
    )
    .execute(&pool)
    .await
    .unwrap();
    let stale = olp::observability::workers::worker_task_health(&pool)
        .await
        .unwrap();
    assert_eq!(
        stale
            .tasks
            .iter()
            .find(|task| task.task == WorkerTask::RequestMetadataConsumer)
            .unwrap()
            .state,
        WorkerTaskState::Stale
    );
    assert_eq!(
        olp::observability::workers::worker_recovery_counters(&(pool),)
            .await
            .unwrap(),
        counters
    );
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make integration"]
async fn maintenance_repeats_committed_retention_batches() {
    let db = TestDb::create_migrated("maintenance_batches").await;
    let pool = db.pool(2).await;
    let expired_rows = 50_001_i64;
    sqlx::query("CREATE TABLE maintenance_batch_transactions (transaction_id xid8 PRIMARY KEY)")
        .execute(&pool)
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
    .execute(&pool)
    .await
    .unwrap();
    sqlx::query(
        "CREATE TRIGGER record_maintenance_batch_transaction \
         AFTER DELETE ON audit_events FOR EACH STATEMENT \
         EXECUTE FUNCTION record_maintenance_batch_transaction()",
    )
    .execute(&pool)
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO audit_events (id, action, resource_type, outcome, occurred_at) \
         SELECT uuidv7(), 'batch-test', 'batch-test', 'succeeded', \
                now() - interval '366 days' \
         FROM generate_series(1, $1)",
    )
    .bind(expired_rows)
    .execute(&pool)
    .await
    .unwrap();

    let report = olp::usage::retention::run_maintenance(&pool, Utc::now())
        .await
        .unwrap();

    assert!(report.lock_acquired);
    assert_eq!(report.audit_rows, expired_rows as u64);
    let (retained, transactions): (i64, i64) = sqlx::query_as(
        "SELECT (SELECT count(*) FROM audit_events), \
                (SELECT count(*) FROM maintenance_batch_transactions)",
    )
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(retained, 0);
    assert_eq!(transactions, 2);
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make integration"]
async fn three_maintenance_replicas_never_overlap_a_destructive_pass() {
    let db = TestDb::create_migrated("maintenance_three_replicas").await;
    let pool = db.pool(12).await;

    // Keep the elected pass blocked immediately after it acquires the
    // session-level advisory lock. The other two replicas must skip
    // instead of queuing a second destructive transaction.
    let mut table_blocker = pool.begin().await.unwrap();
    sqlx::query("LOCK TABLE settings IN ACCESS EXCLUSIVE MODE")
        .execute(&mut *table_blocker)
        .await
        .unwrap();
    let first_store = pool.clone();
    let first = tokio::spawn(async move {
        olp::usage::retention::run_maintenance(&first_store, Utc::now())
            .await
            .unwrap()
    });

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
            .fetch_one(&pool)
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

    let second_store = pool.clone();
    let third_store = pool.clone();
    let second = tokio::spawn(async move {
        olp::usage::retention::run_maintenance(&second_store, Utc::now())
            .await
            .unwrap()
    });
    let third = tokio::spawn(async move {
        olp::usage::retention::run_maintenance(&third_store, Utc::now())
            .await
            .unwrap()
    });
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
#[ignore = "requires PostgreSQL via make integration"]
async fn three_epoch_detectors_record_each_stale_epoch_once() {
    let db = TestDb::create_migrated("epoch_detection_three_replicas").await;
    let pool = db.pool(12).await;
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
    .execute(&pool)
    .await
    .unwrap();

    let candidates = run_three_epoch_detectors(&pool, now).await;
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

    let detections = run_three_epoch_detectors(&pool, now + Duration::seconds(11)).await;
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
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(gap_count, 1);
}

async fn run_three_epoch_detectors(
    pool: &sqlx::PgPool,
    now: chrono::DateTime<Utc>,
) -> Vec<olp::usage::ingestion::reconciliation::EpochDetection> {
    let barrier = Arc::new(Barrier::new(4));
    let mut workers = tokio::task::JoinSet::new();
    for _ in 0..3 {
        let pool = pool.clone();
        let barrier = Arc::clone(&barrier);
        workers.spawn(async move {
            barrier.wait().await;
            olp::usage::ingestion::reconciliation::detect_stale_request_metadata_gateway_epochs(
                &pool, now,
            )
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
