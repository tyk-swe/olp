use super::*;

// E8: verification happens in Rust, so truncating to `limit` in SQL let a run
// of corrupt releases hide every intact one behind it.
#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn a_run_of_corrupt_releases_does_not_hide_an_intact_older_one() {
    let db = olp::test_support::TestDb::create_migrated("release_scan").await;
    let pool = db.pool(3).await;
    let actor = owner_id(&pool, "release-scan").await;
    let valid = olp::runtime::publication::compiler::compile_and_publish_runtime(&pool, actor)
        .await
        .unwrap();

    for _ in 0..4 {
        sqlx::query(
            "INSERT INTO runtime_generations (id, compiled_release, release_sha256, created_by) \
             VALUES ($1, 'corrupt'::text::bytea, $2, $3)",
        )
        .bind(Uuid::now_v7())
        .bind([0_u8; 32].as_slice())
        .bind(actor)
        .execute(&pool)
        .await
        .unwrap();
    }

    let releases =
        olp::runtime::publication::releases::recent_valid_runtime_releases_after(&pool, 2, None)
            .await
            .unwrap();
    assert_eq!(releases.len(), 1);
    assert_eq!(releases[0].generation_id, valid.generation_id);
    assert_eq!(releases[0].sequence, valid.sequence);
}

// E9: worker checkpoints are stamped with the database's clock, so their ages
// have to be measured against that same clock rather than the reader's.
#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn worker_task_age_is_measured_by_the_database_clock() {
    let db = olp::test_support::TestDb::create_migrated("worker_age").await;
    let pool = db.pool(3).await;
    olp::observability::workers::report_worker_task_checkpoint(
        &pool,
        WorkerTask::Maintenance,
        WorkerTaskCheckpointOutcome::Success,
        true,
    )
    .await
    .unwrap();

    let fresh = olp::observability::workers::worker_task_health(&pool)
        .await
        .unwrap();
    let maintenance = fresh
        .tasks
        .iter()
        .find(|task| task.task == WorkerTask::Maintenance)
        .unwrap();
    assert_eq!(maintenance.state, WorkerTaskState::Healthy);
    assert!(maintenance.heartbeat_age_seconds.unwrap() < 60);

    sqlx::query(
        "UPDATE worker_task_health \
         SET checked_at = checked_at - interval '10 minutes', \
             last_success_at = last_success_at - interval '10 minutes' \
         WHERE task = 'maintenance'",
    )
    .execute(&pool)
    .await
    .unwrap();
    let aged = olp::observability::workers::worker_task_health(&pool)
        .await
        .unwrap();
    let maintenance = aged
        .tasks
        .iter()
        .find(|task| task.task == WorkerTask::Maintenance)
        .unwrap();
    assert_eq!(maintenance.state, WorkerTaskState::Stale);
    assert!(maintenance.heartbeat_age_seconds.unwrap() >= 600);
    assert!(maintenance.last_success_age_seconds.unwrap() >= 600);
}

// E13: taking over from a dead owner is not publish progress. Advancing the
// progress clock there makes a crash-looping publisher look healthy forever.
#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn taking_over_a_dead_outbox_owner_is_not_publish_progress() {
    let db = olp::test_support::TestDb::create_migrated("outbox_progress").await;
    let pool = db.pool(4).await;

    let leader = olp::runtime::publication::outbox::acquire_runtime_outbox_leader(&pool)
        .await
        .unwrap();
    leader.release().await.unwrap();
    sqlx::query("UPDATE runtime_outbox_health SET owner_active = true WHERE singleton")
        .execute(&pool)
        .await
        .unwrap();

    let successor = olp::runtime::publication::outbox::acquire_runtime_outbox_leader(&pool)
        .await
        .unwrap();
    let counters = olp::observability::workers::worker_recovery_counters(&pool)
        .await
        .unwrap();
    assert_eq!(counters.runtime_outbox_abandoned_ownership, 1);
    let status = olp::runtime::publication::outbox::runtime_outbox_status(&pool)
        .await
        .unwrap();
    assert!(status.owner_active);
    assert!(
        status.last_progress_at.is_none(),
        "a takeover must not report progress the publisher never made"
    );
    let tasks = olp::observability::workers::worker_task_health(&pool)
        .await
        .unwrap();
    assert!(
        tasks
            .tasks
            .iter()
            .find(|task| task.task == WorkerTask::RuntimeOutbox)
            .unwrap()
            .last_progress_at
            .is_none()
    );
    successor.release().await.unwrap();
}
