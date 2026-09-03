use super::*;

// E14: an entry still in this consumer's PEL whose payload has been deleted
// from the stream is stream loss, not a producer writing a bad event. The
// XAUTOCLAIM path already files it that way; the own-PEL read must agree, or
// operators chase a producer bug instead of stream deletion.
#[tokio::test]
#[ignore = "requires PostgreSQL 18 and Valkey in OLP_VALKEY_URL"]
async fn a_vanished_own_pending_payload_is_recorded_as_a_missing_stream_event() {
    let db = olp_db::test_support::TestDb::create_migrated("metadata_vanished").await;
    let store = db.store(5).await;
    let fixture = fixture(&store, "vanished").await;
    let stream = stream("vanished");
    let mut connection = valkey_connection().await;
    create_group(&mut connection, &stream).await;

    let (id, _) = add_event(&mut connection, &stream, &event(&fixture)).await;
    assert_eq!(
        deliver(&mut connection, &stream, "vanished-owner", 1).await,
        [id.as_str()]
    );
    let deleted: usize = connection.xdel(&stream, &[id.as_str()]).await.unwrap();
    assert_eq!(deleted, 1);

    let (shutdown, receiver) = watch::channel(false);
    let consumer = spawn_consumer(
        store.clone(),
        stream.clone(),
        "vanished-owner",
        receiver,
        test_policy(Duration::from_secs(3600)),
    );
    wait_for_gap(&store, "missing_stream_event").await;
    stop_consumers(shutdown, vec![consumer]).await;

    let malformed: i64 = sqlx::query_scalar(
        "SELECT count(*)::bigint FROM request_metadata_ingestion_gaps \
         WHERE reason = 'malformed_stream_event'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(
        malformed, 0,
        "a deleted payload is stream loss, not a malformed event"
    );
}

// E10: XAUTOCLAIM has already destroyed the evidence for its deleted IDs. The
// gap rows must be written before anything that can fail, and a purely
// informational counter must never abort the batch ahead of them.
#[tokio::test]
#[ignore = "requires PostgreSQL 18 and Valkey in OLP_VALKEY_URL"]
async fn reclaim_gaps_are_recorded_even_when_the_activity_counter_write_fails() {
    let db = olp_db::test_support::TestDb::create_migrated("metadata_reclaim").await;
    let store = db.store(5).await;
    let fixture = fixture(&store, "reclaim-gap").await;
    let stream = stream("reclaim-gap");
    let mut connection = valkey_connection().await;
    create_group(&mut connection, &stream).await;

    // One entry survives so the claim page is non-empty and reports the
    // reclaim counter; one is deleted so the server hands back a deleted ID.
    let (deleted_id, _) = add_event(&mut connection, &stream, &event(&fixture)).await;
    let (live_id, _) = add_event(&mut connection, &stream, &event(&fixture)).await;
    assert_eq!(
        deliver(&mut connection, &stream, "dead-reclaim-owner", 2).await,
        [deleted_id.clone(), live_id]
    );
    let deleted: usize = connection
        .xdel(&stream, &[deleted_id.as_str()])
        .await
        .unwrap();
    assert_eq!(deleted, 1);

    // Break the counter table. The reclaim counter is telemetry; losing it
    // must not cost the durable gap evidence.
    sqlx::query(
        "CREATE FUNCTION reject_worker_counters() RETURNS trigger AS $$ \
         BEGIN RAISE EXCEPTION 'counter write rejected'; END; $$ LANGUAGE plpgsql",
    )
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "CREATE TRIGGER reject_worker_counters BEFORE INSERT OR UPDATE ON async_worker_counters \
         FOR EACH STATEMENT EXECUTE FUNCTION reject_worker_counters()",
    )
    .execute(store.pool())
    .await
    .unwrap();

    let (shutdown, receiver) = watch::channel(false);
    let consumer = spawn_consumer(
        store.clone(),
        stream.clone(),
        "reclaim-survivor",
        receiver,
        test_policy(Duration::ZERO),
    );
    wait_for_gap(&store, "missing_stream_event").await;
    // The surviving entry's own processing counter still fails hard, so the
    // consumer is expected to exit with an error here. What matters is that
    // the gap evidence was already durable before that happened.
    let _ = shutdown.send(true);
    consumer.abort();
    let _ = consumer.await;
}

#[tokio::test]
#[ignore = "requires PostgreSQL 18 and Valkey in OLP_VALKEY_URL"]
async fn reclaim_gap_survives_a_postgres_reporting_failure() {
    let db = olp_db::test_support::TestDb::create_migrated("metadata_reclaim_retry").await;
    let store = db.store(5).await;
    let fixture = fixture(&store, "reclaim-retry").await;
    let stream = stream("reclaim-retry");
    let mut connection = valkey_connection().await;
    create_group(&mut connection, &stream).await;

    let (deleted_id, _) = add_event(&mut connection, &stream, &event(&fixture)).await;
    assert_eq!(
        deliver(&mut connection, &stream, "dead-reclaim-retry-owner", 1).await,
        std::slice::from_ref(&deleted_id)
    );
    let deleted: usize = connection
        .xdel(&stream, &[deleted_id.as_str()])
        .await
        .unwrap();
    assert_eq!(deleted, 1);

    sqlx::query(
        "CREATE FUNCTION reject_request_metadata_gap() RETURNS trigger AS $$ \
         BEGIN RAISE EXCEPTION 'gap write rejected'; END; $$ LANGUAGE plpgsql",
    )
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "CREATE TRIGGER reject_request_metadata_gap BEFORE INSERT \
         ON request_metadata_ingestion_gaps FOR EACH ROW \
         EXECUTE FUNCTION reject_request_metadata_gap()",
    )
    .execute(store.pool())
    .await
    .unwrap();

    let (_failed_shutdown, failed_receiver) = watch::channel(false);
    let failed_consumer = spawn_consumer(
        store.clone(),
        stream.clone(),
        "reclaim-retry-first",
        failed_receiver,
        test_policy(Duration::ZERO),
    );
    let failed = tokio::time::timeout(WAIT_TIMEOUT, failed_consumer)
        .await
        .expect("consumer must return the rejected gap write")
        .unwrap();
    assert!(failed.is_err());
    let marker_count: usize = connection.xlen(&stream).await.unwrap();
    assert_eq!(marker_count, 1);

    sqlx::query("DROP TRIGGER reject_request_metadata_gap ON request_metadata_ingestion_gaps")
        .execute(store.pool())
        .await
        .unwrap();
    sqlx::query("DROP FUNCTION reject_request_metadata_gap()")
        .execute(store.pool())
        .await
        .unwrap();

    let (shutdown, receiver) = watch::channel(false);
    let consumer = spawn_consumer(
        store.clone(),
        stream.clone(),
        "reclaim-retry-second",
        receiver,
        test_policy(Duration::ZERO),
    );
    wait_for_gap(&store, "missing_stream_event").await;
    stop_consumers(shutdown, vec![consumer]).await;
}
