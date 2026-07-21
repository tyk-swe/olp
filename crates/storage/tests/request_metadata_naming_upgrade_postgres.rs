use olp_storage::{MIGRATOR, PgStore};
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn request_metadata_schema_rename_preserves_legacy_rows() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 3).await.unwrap();
    MIGRATOR.run_to(27, store.pool()).await.unwrap();

    let gap_id = Uuid::now_v7();
    let process_epoch = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO usage_ingestion_gaps \
         (id, gateway_instance, event_count, reason, certainty, first_observed_at, \
          last_observed_at, deduplication_key) \
          VALUES ($1, 'gateway-a', 1, 'fresh_valkey_stream_loss_exact:incident-1', \
                  'exact', now(), now(), 'fresh_valkey_stream_loss_exact:incident-1')",
    )
    .bind(gap_id)
    .execute(store.pool())
    .await
    .unwrap();
    let mut transaction = store.pool().begin().await.unwrap();
    sqlx::query("SELECT set_config('olp.usage_rollup_writer', 'additive-v2', true)")
        .execute(&mut *transaction)
        .await
        .unwrap();
    sqlx::query(
        "INSERT INTO usage_gap_hourly \
         (bucket, gateway_instance, reason, event_count, first_observed_at, \
          last_observed_at, uncertain_gap_count) \
          VALUES (date_trunc('hour', now()), 'gateway-a', \
                  'fresh_valkey_stream_loss_exact:incident-1', 1, \
                 date_trunc('hour', now()), now(), 0)",
    )
    .execute(&mut *transaction)
    .await
    .unwrap();
    transaction.commit().await.unwrap();
    sqlx::query(
        "INSERT INTO usage_event_receipts \
         (event_id, request_id, event_sha256, status, observed_at) \
         VALUES ($1, $2, $3, 'fact_persisted', now())",
    )
    .bind(Uuid::now_v7())
    .bind(Uuid::now_v7())
    .bind([7_u8; 32].as_slice())
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO usage_consumer_health \
         (singleton, pending_events, lag_events, checked_at) VALUES (true, 0, 0, now())",
    )
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO usage_loss_reporter_state \
         (gateway_instance, process_epoch, dropped, abandoned, updated_at) \
         VALUES ('gateway-a', $1, 1, 0, now())",
    )
    .bind(process_epoch)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO usage_gateway_epochs \
         (gateway_instance, process_epoch, started_at, accepted, persisted, dropped, \
          abandoned, retrying, writer_closed, updated_at, uncertainty_gap_id) \
         VALUES ('gateway-a', $1, now(), 2, 1, 1, 0, false, false, now(), $2)",
    )
    .bind(process_epoch)
    .bind(gap_id)
    .execute(store.pool())
    .await
    .unwrap();

    MIGRATOR.run(store.pool()).await.unwrap();

    let table_counts: Vec<(String, i64)> = sqlx::query_as(
        "SELECT 'request_metadata_event_receipts', count(*) FROM request_metadata_event_receipts \
         UNION ALL SELECT 'request_metadata_consumer_health', count(*) FROM request_metadata_consumer_health \
         UNION ALL SELECT 'request_metadata_gateway_epochs', count(*) FROM request_metadata_gateway_epochs \
         UNION ALL SELECT 'request_metadata_ingestion_gaps', count(*) FROM request_metadata_ingestion_gaps \
         UNION ALL SELECT 'request_metadata_gap_hourly', count(*) FROM request_metadata_gap_hourly \
         UNION ALL SELECT 'request_metadata_loss_reporter_state', count(*) FROM request_metadata_loss_reporter_state",
    )
    .fetch_all(store.pool())
    .await
    .unwrap();
    for (table, count) in table_counts {
        assert_eq!(count, 1, "{table} did not preserve its row");
    }

    let (gap_reason, deduplication_key): (String, Option<String>) = sqlx::query_as(
        "SELECT reason, deduplication_key FROM request_metadata_ingestion_gaps WHERE id = $1",
    )
    .bind(gap_id)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(gap_reason, "request_metadata_stream_loss_exact:incident-1");
    assert_eq!(deduplication_key.as_deref(), Some(gap_reason.as_str()));
    let hourly_reason: String =
        sqlx::query_scalar("SELECT reason FROM request_metadata_gap_hourly")
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(hourly_reason, gap_reason);

    let old_relations: i64 =
        sqlx::query_scalar("SELECT count(*) FROM pg_class WHERE relname = ANY($1::text[])")
            .bind([
                "usage_event_receipts",
                "usage_consumer_health",
                "usage_gateway_epochs",
                "usage_ingestion_gaps",
                "usage_gap_hourly",
                "usage_loss_reporter_state",
            ])
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(old_relations, 0);

    let renamed_types: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM pg_type \
         WHERE typname IN ('request_metadata_event_receipt_status', \
                           'request_metadata_gap_certainty')",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(renamed_types, 2);
    let old_types: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM pg_type \
         WHERE typname IN ('usage_event_receipt_status', 'usage_gap_certainty')",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(old_types, 0);

    let renamed_indexes: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM pg_class \
         WHERE relkind = 'i' AND relname IN ( \
           'request_metadata_event_receipts_recorded_at_idx', \
           'request_metadata_gateway_epochs_one_open_idx', \
           'request_metadata_gateway_epochs_process_epoch_idx', \
           'request_metadata_gateway_epochs_stale_scan_idx', \
           'request_metadata_gateway_epochs_unresolved_idx', \
           'request_metadata_ingestion_gaps_deduplication_key_idx', \
           'request_metadata_gap_hourly_overlap_idx')",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(renamed_indexes, 7);

    let old_constraints: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM pg_constraint \
         WHERE conname LIKE 'usage\\_%' ESCAPE '\\' \
           AND conrelid IN ( \
             'request_metadata_event_receipts'::regclass, \
             'request_metadata_consumer_health'::regclass, \
             'request_metadata_gateway_epochs'::regclass, \
             'request_metadata_ingestion_gaps'::regclass, \
             'request_metadata_gap_hourly'::regclass, \
             'request_metadata_loss_reporter_state'::regclass)",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(old_constraints, 0);

    let renamed_functions: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM pg_proc \
         WHERE proname IN ('enforce_request_metadata_fact_receipt', \
                           'preserve_request_metadata_fact_receipt')",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(renamed_functions, 2);
    let renamed_triggers: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM pg_trigger \
         WHERE tgname IN ('usage_facts_request_metadata_receipt_guard', \
                          'usage_facts_preserve_request_metadata_receipt', \
                          'request_metadata_gap_hourly_writer_guard')",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(renamed_triggers, 3);
}
