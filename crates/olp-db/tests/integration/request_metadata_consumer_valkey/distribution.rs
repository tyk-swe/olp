use super::*;

#[tokio::test]
#[ignore = "requires PostgreSQL 18 and Valkey in OLP_VALKEY_URL"]
async fn idle_production_consumer_processes_after_a_full_blocking_read() {
    let db = olp_db::test_support::TestDb::create_migrated("metadata_idle_block").await;
    let store = db.store(5).await;
    let fixture = fixture(&store, "idle-production-block").await;
    let stream = stream("idle-production-block");
    let mut connection = valkey_connection().await;
    let (shutdown, receiver) = watch::channel(false);
    let consumer_store = store.clone();
    let consumer_stream = stream.clone();
    let consumer = tokio::spawn(async move {
        olp_db::valkey::request_metadata::run_request_metadata_consumer(
            &consumer_store,
            &valkey_url(),
            &consumer_stream,
            "idle-production-consumer",
            &limits_namespace(&consumer_stream),
            receiver,
        )
        .await
    });

    wait_for_consumers(&mut connection, &stream, &["idle-production-consumer"]).await;
    tokio::time::sleep(Duration::from_millis(1_500)).await;
    assert!(
        !consumer.is_finished(),
        "an idle consumer must outlive its one-second blocking read"
    );
    add_event(&mut connection, &stream, &event(&fixture)).await;
    wait_for_usage_facts(&store, 1).await;
    stop_consumers(shutdown, vec![consumer]).await;
}

#[tokio::test]
#[ignore = "requires PostgreSQL 18 and Valkey in OLP_VALKEY_URL"]
async fn three_workers_distribute_new_events_and_checkpoint_group_wide_drain() {
    let db = olp_db::test_support::TestDb::create_migrated("metadata_ha").await;
    let store = db.store(12).await;
    let fixture = fixture(&store, "three-workers").await;
    let stream = stream("three-workers");
    let mut connection = valkey_connection().await;
    let mut persistence_lock = store.pool().begin().await.unwrap();
    sqlx::query("LOCK TABLE request_metadata_event_receipts IN ACCESS EXCLUSIVE MODE")
        .execute(&mut *persistence_lock)
        .await
        .unwrap();
    let (shutdown, receiver) = watch::channel(false);
    let policy = RequestMetadataConsumerTestPolicy {
        batch_size: 1,
        ..test_policy(Duration::from_secs(60))
    };
    let consumers = ["worker-a", "worker-b", "worker-c"]
        .into_iter()
        .map(|consumer| {
            spawn_consumer(
                store.clone(),
                stream.clone(),
                consumer,
                receiver.clone(),
                policy,
            )
        })
        .collect::<Vec<_>>();
    wait_for_consumers(
        &mut connection,
        &stream,
        &["worker-a", "worker-b", "worker-c"],
    )
    .await;

    for _ in 0..3 {
        add_event(&mut connection, &stream, &event(&fixture)).await;
    }
    tokio::time::timeout(WAIT_TIMEOUT, async {
        let mut interval = tokio::time::interval(Duration::from_millis(10));
        loop {
            interval.tick().await;
            let pending: StreamPendingCountReply = connection
                .xpending_count(&stream, GROUP, "-", "+", 10)
                .await
                .unwrap();
            let owners = pending
                .ids
                .into_iter()
                .map(|pending| pending.consumer)
                .collect::<std::collections::HashSet<_>>();
            if owners
                == ["worker-a", "worker-b", "worker-c"]
                    .map(str::to_owned)
                    .into()
            {
                return;
            }
        }
    })
    .await
    .expect("one blocked delivery must be distributed to each live worker");
    for _ in 3..24 {
        add_event(&mut connection, &stream, &event(&fixture)).await;
    }
    persistence_lock.rollback().await.unwrap();
    wait_for_usage_facts(&store, 24).await;
    wait_for_group_drain(&store, &mut connection, &stream).await;

    let requests: i64 = sqlx::query_scalar("SELECT count(*) FROM requests")
        .fetch_one(store.pool())
        .await
        .unwrap();
    let receipts: i64 = sqlx::query_scalar("SELECT count(*) FROM request_metadata_event_receipts")
        .fetch_one(store.pool())
        .await
        .unwrap();
    assert_eq!(requests, 24);
    assert_eq!(receipts, 24);
    stop_consumers(shutdown, consumers).await;
}
