use super::*;

#[tokio::test]
#[ignore = "requires PostgreSQL 18 and Valkey in OLP_VALKEY_URL"]
async fn reconnect_resumes_own_pending_before_stale_idle_and_survivor_recovers_dead_owner() {
    let db = olp_db::test_support::TestDb::create_migrated("metadata_reconnect").await;
    let store = db.store(8).await;
    let fixture = fixture(&store, "reconnect").await;
    let stream = stream("reconnect");
    let mut connection = valkey_connection().await;
    create_group(&mut connection, &stream).await;

    let first = event(&fixture);
    let (first_id, _) = add_event(&mut connection, &stream, &first).await;
    assert_eq!(
        deliver(&mut connection, &stream, "same-process", 1).await,
        [first_id]
    );
    drop(connection); // The delivered entry remains in this consumer's PEL.

    let (shutdown, receiver) = watch::channel(false);
    let consumer = spawn_consumer(
        store.clone(),
        stream.clone(),
        "same-process",
        receiver,
        test_policy(Duration::from_secs(60)),
    );
    wait_for_usage_facts(&store, 1).await;
    stop_consumers(shutdown, vec![consumer]).await;

    let mut connection = valkey_connection().await;
    let second = event(&fixture);
    let (second_id, _) = add_event(&mut connection, &stream, &second).await;
    assert_eq!(
        deliver(&mut connection, &stream, "dead-process", 1).await,
        [second_id]
    );
    let (shutdown, receiver) = watch::channel(false);
    let survivor = spawn_consumer(
        store.clone(),
        stream.clone(),
        "survivor",
        receiver,
        test_policy(Duration::ZERO),
    );
    wait_for_usage_facts(&store, 2).await;
    wait_for_group_drain(&store, &mut connection, &stream).await;
    stop_consumers(shutdown, vec![survivor]).await;

    // Hold the first receipt insert so the real consumer is known to be
    // between stream delivery and PostgreSQL persistence, then hard-abort it.
    // The survivor must claim the abandoned PEL entry and commit it once.
    let mut persistence_lock = store.pool().begin().await.unwrap();
    sqlx::query("LOCK TABLE request_metadata_event_receipts IN ACCESS EXCLUSIVE MODE")
        .execute(&mut *persistence_lock)
        .await
        .unwrap();
    let (doomed_shutdown, doomed_receiver) = watch::channel(false);
    let doomed = spawn_consumer(
        store.clone(),
        stream.clone(),
        "hard-killed-process",
        doomed_receiver,
        test_policy(Duration::from_secs(60)),
    );
    wait_for_consumers(&mut connection, &stream, &["hard-killed-process"]).await;
    let third = event(&fixture);
    let (third_id, _) = add_event(&mut connection, &stream, &third).await;
    tokio::time::timeout(WAIT_TIMEOUT, async {
        let mut interval = tokio::time::interval(Duration::from_millis(10));
        loop {
            interval.tick().await;
            if pending_owner(&mut connection, &stream, &third_id)
                .await
                .as_deref()
                == Some("hard-killed-process")
            {
                return;
            }
        }
    })
    .await
    .expect("the doomed worker must receive the stream entry");
    doomed.abort();
    assert!(
        tokio::time::timeout(WAIT_TIMEOUT, doomed)
            .await
            .unwrap()
            .unwrap_err()
            .is_cancelled()
    );
    drop(doomed_shutdown);
    persistence_lock.rollback().await.unwrap();

    let (shutdown, receiver) = watch::channel(false);
    let survivor = spawn_consumer(
        store.clone(),
        stream.clone(),
        "hard-kill-survivor",
        receiver,
        test_policy(Duration::ZERO),
    );
    wait_for_usage_facts(&store, 3).await;
    wait_for_group_drain(&store, &mut connection, &stream).await;
    stop_consumers(shutdown, vec![survivor]).await;

    // Kill the worker's Valkey socket while its PostgreSQL insert is blocked.
    // It may reconnect transparently or restart with this stable identity, but
    // it must not acknowledge before the durable commit.
    let mut persistence_lock = store.pool().begin().await.unwrap();
    sqlx::query("LOCK TABLE request_metadata_event_receipts IN ACCESS EXCLUSIVE MODE")
        .execute(&mut *persistence_lock)
        .await
        .unwrap();
    let (shutdown, receiver) = watch::channel(false);
    let mut connection_loss_worker = spawn_consumer(
        store.clone(),
        stream.clone(),
        "connection-loss-process",
        receiver,
        test_policy(Duration::from_secs(60)),
    );
    wait_for_consumers(&mut connection, &stream, &["connection-loss-process"]).await;
    let fourth = event(&fixture);
    let (fourth_id, _) = add_event(&mut connection, &stream, &fourth).await;
    tokio::time::timeout(WAIT_TIMEOUT, async {
        let mut interval = tokio::time::interval(Duration::from_millis(10));
        loop {
            interval.tick().await;
            if pending_owner(&mut connection, &stream, &fourth_id)
                .await
                .as_deref()
                == Some("connection-loss-process")
            {
                return;
            }
        }
    })
    .await
    .expect("the connection-loss worker must be processing the entry");
    kill_consumer_connection(&mut connection, "connection-loss-process").await;
    persistence_lock.rollback().await.unwrap();
    wait_for_usage_facts(&store, 4).await;
    let consumers = tokio::select! {
        biased;
        result = &mut connection_loss_worker => {
            assert!(result.unwrap().is_err(), "a consumer may only return successfully on shutdown");
            vec![spawn_consumer(
                store.clone(),
                stream.clone(),
                "connection-loss-process",
                shutdown.subscribe(),
                test_policy(Duration::from_secs(60)),
            )]
        }
        () = wait_for_valkey_drain(&mut connection, &stream) => {
            vec![connection_loss_worker]
        }
    };
    wait_for_group_drain(&store, &mut connection, &stream).await;
    stop_consumers(shutdown, consumers).await;
}

#[tokio::test]
#[ignore = "requires PostgreSQL 18 and Valkey in OLP_VALKEY_URL"]
async fn commit_before_ack_replays_as_duplicate_under_two_concurrent_claimants() {
    let db = olp_db::test_support::TestDb::create_migrated("metadata_commit").await;
    let store = db.store(10).await;
    let fixture = fixture(&store, "commit-replay").await;
    let stream = stream("commit-replay");
    let mut connection = valkey_connection().await;
    create_group(&mut connection, &stream).await;
    let event = event(&fixture);
    let (id, payload) = add_event(&mut connection, &stream, &event).await;
    assert_eq!(
        deliver(&mut connection, &stream, "committed-owner", 1).await,
        [id]
    );

    let persisted = store
        .persist_request_metadata_stream_event(&event, &payload)
        .await
        .unwrap();
    assert_eq!(persisted.outcome, Outcome::Persisted);
    let first_snapshot = persisted.cost_snapshot.unwrap();
    assert_eq!(first_snapshot.unpriced_attempts, 1);

    let (shutdown, receiver) = watch::channel(false);
    let barrier = Arc::new(tokio::sync::Barrier::new(3));
    let mut consumers = Vec::new();
    for consumer in ["claimant-a", "claimant-b"] {
        let store = store.clone();
        let stream = stream.clone();
        let receiver = receiver.clone();
        let barrier = Arc::clone(&barrier);
        consumers.push(tokio::spawn(async move {
            barrier.wait().await;
            run_request_metadata_consumer(
                &store,
                &valkey_url(),
                &stream,
                consumer,
                &limits_namespace(&stream),
                receiver,
                test_policy(Duration::ZERO),
            )
            .await
        }));
    }
    barrier.wait().await;
    wait_for_group_drain(&store, &mut connection, &stream).await;

    let duplicate = store
        .persist_request_metadata_stream_event(&event, &payload)
        .await
        .unwrap();
    assert_eq!(duplicate.outcome, Outcome::Duplicate);
    assert_eq!(duplicate.cost_snapshot, None);
    let counts: (i64, i64, i64) = sqlx::query_as(
        "SELECT (SELECT count(*) FROM requests), \
                (SELECT count(*) FROM usage_facts), \
                (SELECT count(*) FROM request_metadata_event_receipts)",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(counts, (1, 1, 1));
    let second_event = self::event(&fixture);
    let second_payload = serde_json::to_vec(&second_event).unwrap();
    let second = store
        .persist_request_metadata_stream_event(&second_event, &second_payload)
        .await
        .unwrap();
    let second_snapshot = second.cost_snapshot.unwrap();
    assert_eq!(
        second_snapshot.monthly_window_id,
        first_snapshot.monthly_window_id
    );
    assert_eq!(second_snapshot.unpriced_attempts, 2);
    let cost_limiter = DistributedLimiter::connect(&valkey_url(), limits_namespace(&stream))
        .await
        .unwrap();
    cost_limiter
        .apply_cost_snapshot(&second_snapshot)
        .await
        .unwrap();
    cost_limiter
        .apply_cost_snapshot(&second_snapshot)
        .await
        .unwrap();
    let monthly_cost_key = format!(
        "{}:{{{}}}:cost:month",
        limits_namespace(&stream),
        fixture.api_key_id.simple()
    );
    assert_eq!(
        connection
            .hget::<_, _, i64>(monthly_cost_key, "unpriced")
            .await
            .unwrap(),
        2
    );
    stop_consumers(shutdown, consumers).await;
    let counters = store.worker_recovery_counters().await.unwrap();
    // With a zero-idle test policy both contenders may transfer the same PEL
    // entry before the winner acknowledges it. The counter records recovery
    // activity, not unique event identity.
    assert!(counters.request_metadata_reclaimed >= 1);
    assert!(counters.request_metadata_recovered >= 1);
    assert_eq!(
        counters.request_metadata_duplicates,
        counters.request_metadata_recovered
    );
    assert_eq!(
        counters.request_metadata_processed,
        counters.request_metadata_recovered
    );
}

#[tokio::test]
#[ignore = "requires PostgreSQL 18 and Valkey in OLP_VALKEY_URL"]
async fn slow_active_entry_is_not_stolen_below_idle_threshold() {
    let db = olp_db::test_support::TestDb::create_migrated("metadata_idle").await;
    let store = db.store(8).await;
    let fixture = fixture(&store, "idle-threshold").await;
    let stream = stream("idle-threshold");
    let mut connection = valkey_connection().await;
    create_group(&mut connection, &stream).await;
    let mut persistence_lock = store.pool().begin().await.unwrap();
    sqlx::query("LOCK TABLE request_metadata_event_receipts IN ACCESS EXCLUSIVE MODE")
        .execute(&mut *persistence_lock)
        .await
        .unwrap();
    let (shutdown, receiver) = watch::channel(false);
    let slow = spawn_consumer(
        store.clone(),
        stream.clone(),
        "slow-active",
        receiver,
        test_policy(Duration::from_secs(60)),
    );
    wait_for_consumers(&mut connection, &stream, &["slow-active"]).await;
    let event = event(&fixture);
    let (id, _) = add_event(&mut connection, &stream, &event).await;
    tokio::time::timeout(WAIT_TIMEOUT, async {
        let mut interval = tokio::time::interval(Duration::from_millis(10));
        loop {
            interval.tick().await;
            if pending_owner(&mut connection, &stream, &id)
                .await
                .as_deref()
                == Some("slow-active")
            {
                return;
            }
        }
    })
    .await
    .unwrap();

    let claim: StreamAutoClaimReply = connection
        .xautoclaim_options(
            &stream,
            GROUP,
            "premature-claimant",
            60_000,
            "0-0",
            StreamAutoClaimOptions::default().count(10),
        )
        .await
        .unwrap();
    assert!(claim.claimed.is_empty());
    assert_eq!(
        pending_owner(&mut connection, &stream, &id)
            .await
            .as_deref(),
        Some("slow-active")
    );
    let fact_count: i64 = sqlx::query_scalar("SELECT count(*) FROM usage_facts")
        .fetch_one(store.pool())
        .await
        .unwrap();
    assert_eq!(fact_count, 0);
    persistence_lock.rollback().await.unwrap();
    wait_for_usage_facts(&store, 1).await;
    wait_for_group_drain(&store, &mut connection, &stream).await;
    stop_consumers(shutdown, vec![slow]).await;
}

#[tokio::test]
#[ignore = "requires PostgreSQL 18 and Valkey in OLP_VALKEY_URL"]
async fn concurrent_recovery_records_malformed_and_invalid_entries_once_then_drains() {
    let db = olp_db::test_support::TestDb::create_migrated("metadata_invalid").await;
    let store = db.store(10).await;
    let fixture = fixture(&store, "invalid-recovery").await;
    let stream = stream("invalid-recovery");
    let mut connection = valkey_connection().await;
    create_group(&mut connection, &stream).await;

    add_payload(&mut connection, &stream, b"not-json").await;
    let mut invalid = event(&fixture);
    invalid.route_slug.clear();
    add_event(&mut connection, &stream, &invalid).await;
    assert_eq!(
        deliver(&mut connection, &stream, "dead-invalid-owner", 2)
            .await
            .len(),
        2
    );

    let (shutdown, receiver) = watch::channel(false);
    let barrier = Arc::new(tokio::sync::Barrier::new(3));
    let mut consumers = Vec::new();
    for consumer in ["invalid-claimant-a", "invalid-claimant-b"] {
        let store = store.clone();
        let stream = stream.clone();
        let receiver = receiver.clone();
        let barrier = Arc::clone(&barrier);
        consumers.push(tokio::spawn(async move {
            barrier.wait().await;
            run_request_metadata_consumer(
                &store,
                &valkey_url(),
                &stream,
                consumer,
                &limits_namespace(&stream),
                receiver,
                test_policy(Duration::ZERO),
            )
            .await
        }));
    }
    barrier.wait().await;
    wait_for_group_drain(&store, &mut connection, &stream).await;

    let gaps: Vec<(String, i64, i64)> = sqlx::query_as(
        "SELECT reason, count(*), sum(event_count)::bigint \
         FROM request_metadata_ingestion_gaps \
         WHERE reason IN ('malformed_stream_event', 'invalid_request_metadata_event') \
         GROUP BY reason ORDER BY reason",
    )
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert_eq!(
        gaps,
        [
            ("invalid_request_metadata_event".to_owned(), 1, 1),
            ("malformed_stream_event".to_owned(), 1, 1),
        ]
    );
    stop_consumers(shutdown, consumers).await;
}
