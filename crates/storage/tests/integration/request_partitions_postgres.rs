use chrono::{Duration, Utc};
use olp_storage::MIGRATOR;
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn request_partitions_route_ahead_detect_spill_and_drop_with_attempts() {
    let db = olp_storage::test_support::TestDb::create_empty("request_partitions").await;
    let store = db.store(2).await;
    MIGRATOR.run_to(30, store.pool()).await.unwrap();

    let owner_id = Uuid::now_v7();
    let provider_id = Uuid::now_v7();
    let api_key_id = Uuid::now_v7();
    let generation_id = Uuid::now_v7();
    sqlx::query("INSERT INTO installation (installation_name) VALUES ('Partition fixture')")
        .execute(store.pool())
        .await
        .unwrap();
    sqlx::query(
        "INSERT INTO users (id, email, display_name, role) \
         VALUES ($1, 'partition@example.test', 'Partition owner', 'owner')",
    )
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO providers (id, name, kind, auth_mode, etag, created_by) \
         VALUES ($1, 'partition-provider', 'open_ai', 'api_key', $2, $3)",
    )
    .bind(provider_id)
    .bind(Uuid::now_v7())
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO api_keys (id, lookup_id, secret_digest, name, created_by) \
         VALUES ($1, 'olpv2partition1', $2, 'partition key', $3)",
    )
    .bind(api_key_id)
    .bind([7_u8; 32].as_slice())
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO runtime_generations \
         (id, compiled_release, release_sha256, created_by) \
         VALUES ($1, $2, $3, $4)",
    )
    .bind(generation_id)
    .bind([1_u8].as_slice())
    .bind([2_u8; 32].as_slice())
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();

    let legacy_id = Uuid::now_v7();
    let legacy_started_at = Utc::now();
    insert_request(
        store.pool(),
        legacy_id,
        generation_id,
        api_key_id,
        legacy_started_at,
    )
    .await;
    sqlx::query(
        "INSERT INTO usage_request_anchors (request_id, request_started_at) VALUES ($1, $2)",
    )
    .bind(legacy_id)
    .bind(legacy_started_at)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO usage_facts (
            id, request_id, request_started_at, api_key_id, provider_id,
            route_slug, upstream_model, operation, surface, observed_at,
            input_tokens, output_tokens, cached_input_tokens, estimated_cost,
            unpriced, usage_complete
         ) VALUES (
            $1, $2, $3, $4, $5, 'default', 'partition-model', 'generation',
            'openai', $3, 10, 2, 15, NULL, true, true
         )",
    )
    .bind(Uuid::now_v7())
    .bind(legacy_id)
    .bind(legacy_started_at)
    .bind(api_key_id)
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
    let mut usage_rollup = store.pool().begin().await.unwrap();
    sqlx::query("SET LOCAL olp.usage_rollup_writer = 'additive-v2'")
        .execute(&mut *usage_rollup)
        .await
        .unwrap();
    sqlx::query(
        "INSERT INTO usage_hourly (
            bucket, route_slug, provider_id, upstream_model, operation, surface,
            request_count, input_tokens, output_tokens, cached_input_tokens,
            estimated_cost, unpriced_count, incomplete_count
         ) VALUES (
            date_trunc('hour', $1::timestamptz), 'legacy-hourly', $2,
            'partition-model', 'generation', 'openai', 1, 10, 2, 15,
            NULL, 1, 0
         )",
    )
    .bind(legacy_started_at)
    .bind(provider_id)
    .execute(&mut *usage_rollup)
    .await
    .unwrap();
    usage_rollup.commit().await.unwrap();
    sqlx::query("UPDATE api_keys SET tokens_per_minute = 9223372036854775807 WHERE id = $1")
        .bind(api_key_id)
        .execute(store.pool())
        .await
        .unwrap();

    MIGRATOR.run_to(31, store.pool()).await.unwrap();
    assert_eq!(
        request_partition(store.pool(), legacy_id).await,
        "requests_default"
    );

    MIGRATOR.run(store.pool()).await.unwrap();
    let fact_cached: Option<i64> =
        sqlx::query_scalar("SELECT cached_input_tokens FROM usage_facts WHERE request_id = $1")
            .bind(legacy_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(fact_cached, Some(10));
    let hourly_cached: rust_decimal::Decimal = sqlx::query_scalar(
        "SELECT cached_input_tokens FROM usage_hourly WHERE route_slug = 'legacy-hourly'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(hourly_cached, rust_decimal::Decimal::TEN);
    let tokens_per_minute: i64 =
        sqlx::query_scalar("SELECT tokens_per_minute FROM api_keys WHERE id = $1")
            .bind(api_key_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(tokens_per_minute, 9_007_199_254_740_991);
    assert!(
        sqlx::query("UPDATE api_keys SET tokens_per_minute = 9007199254740992 WHERE id = $1")
            .bind(api_key_id)
            .execute(store.pool())
            .await
            .is_err()
    );
    assert_eq!(
        request_partition(store.pool(), legacy_id).await,
        "requests_default"
    );

    let partitions: Vec<(chrono::DateTime<Utc>, chrono::DateTime<Utc>, String)> = sqlx::query_as(
        "SELECT partition_start, partition_end, partition_name \
             FROM request_partitions ORDER BY partition_start",
    )
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert_eq!(partitions.len(), 3);

    let partitioned_id = Uuid::now_v7();
    let partitioned_started_at = partitions[0].0 + Duration::days(1);
    insert_request(
        store.pool(),
        partitioned_id,
        generation_id,
        api_key_id,
        partitioned_started_at,
    )
    .await;
    assert_eq!(
        request_partition(store.pool(), partitioned_id).await,
        partitions[0].2
    );
    sqlx::query(
        "INSERT INTO attempts \
         (id, request_id, request_started_at, ordinal, provider_id, upstream_model, started_at) \
         SELECT gen_random_uuid(), $1, $2, ordinal::smallint, $3, 'partition-model', $2 \
           FROM generate_series(1, 10001) ordinal",
    )
    .bind(partitioned_id)
    .bind(partitioned_started_at)
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();

    // A month missed during a long outage stays safely in the default
    // partition and is surfaced rather than being rewritten under its FK.
    let spill_id = Uuid::now_v7();
    let spill_started_at = partitions[2].1 + Duration::days(1);
    insert_request(
        store.pool(),
        spill_id,
        generation_id,
        api_key_id,
        spill_started_at,
    )
    .await;
    assert_eq!(
        request_partition(store.pool(), spill_id).await,
        "requests_default"
    );
    assert!(
        store
            .request_partition_health()
            .await
            .unwrap()
            .default_spill_detected
    );

    sqlx::query(
        "INSERT INTO settings (key, value, etag, updated_by) \
         VALUES ('retention.requests_days', '1', $1, $2)",
    )
    .bind(Uuid::now_v7())
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    let blocked_default_id = Uuid::now_v7();
    insert_request(
        store.pool(),
        blocked_default_id,
        generation_id,
        api_key_id,
        legacy_started_at,
    )
    .await;
    let blocked_default_attempt_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO attempts \
         (id, request_id, request_started_at, ordinal, provider_id, upstream_model, started_at) \
         VALUES ($1, $2, $3, 1, $4, 'partition-model', $3)",
    )
    .bind(blocked_default_attempt_id)
    .bind(blocked_default_id)
    .bind(legacy_started_at)
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
    let mut locked_attempt = store.pool().begin().await.unwrap();
    sqlx::query("SELECT id FROM attempts WHERE id = $1 FOR UPDATE")
        .bind(blocked_default_attempt_id)
        .fetch_one(&mut *locked_attempt)
        .await
        .unwrap();
    let first_report = store
        .run_maintenance(partitions[0].1 + Duration::days(2))
        .await
        .unwrap();
    assert_eq!(first_report.request_partitions_dropped, 0);
    assert!(
        sqlx::query_scalar::<_, bool>(
            "SELECT EXISTS (SELECT 1 FROM requests_default WHERE id = $1)",
        )
        .bind(blocked_default_id)
        .fetch_one(&mut *locked_attempt)
        .await
        .unwrap()
    );
    locked_attempt.commit().await.unwrap();
    assert_eq!(
        sqlx::query_scalar::<_, i64>("SELECT count(*) FROM attempts WHERE request_id = $1",)
            .bind(partitioned_id)
            .fetch_one(store.pool())
            .await
            .unwrap(),
        1
    );
    let second_report = store
        .run_maintenance(partitions[0].1 + Duration::days(2))
        .await
        .unwrap();
    assert_eq!(second_report.request_partitions_dropped, 1);
    assert!(
        sqlx::query_scalar::<_, bool>(
            "SELECT NOT EXISTS (SELECT 1 FROM requests_default WHERE id = $1)",
        )
        .bind(blocked_default_id)
        .fetch_one(store.pool())
        .await
        .unwrap()
    );
    assert!(
        sqlx::query_scalar::<_, bool>(
            "SELECT NOT EXISTS (SELECT 1 FROM attempts WHERE request_id = $1)",
        )
        .bind(partitioned_id)
        .fetch_one(store.pool())
        .await
        .unwrap()
    );
    assert!(
        sqlx::query_scalar::<_, bool>(
            "SELECT EXISTS (SELECT 1 FROM requests_default WHERE id = $1)",
        )
        .bind(spill_id)
        .fetch_one(store.pool())
        .await
        .unwrap()
    );

    // Catch-up stays bounded instead of holding the maintenance lock while
    // every historical partition is dropped in one tick.
    let catch_up_reference = partitions[2].1 + Duration::days(40);
    sqlx::query_as::<_, (i64, i64)>(
        "SELECT * FROM olp_maintain_request_partitions($1, '-infinity'::timestamptz)",
    )
    .bind(catch_up_reference)
    .fetch_one(store.pool())
    .await
    .unwrap();
    let catch_up_cutoff = catch_up_reference + Duration::days(150);
    let (_, dropped): (i64, i64) =
        sqlx::query_as("SELECT * FROM olp_maintain_request_partitions($1, $2)")
            .bind(catch_up_reference)
            .bind(catch_up_cutoff)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(dropped, 3);
    assert!(
        sqlx::query_scalar::<_, bool>(
            "SELECT EXISTS (\
                 SELECT 1 FROM request_partitions WHERE partition_end <= $1\
             )",
        )
        .bind(catch_up_cutoff)
        .fetch_one(store.pool())
        .await
        .unwrap()
    );

    sqlx::query("DELETE FROM request_partition_state")
        .execute(store.pool())
        .await
        .unwrap();
    assert!(
        store.request_partition_health().await.is_err(),
        "missing partition state must not report a healthy false value"
    );
}

async fn insert_request(
    pool: &sqlx::PgPool,
    request_id: Uuid,
    generation_id: Uuid,
    api_key_id: Uuid,
    started_at: chrono::DateTime<Utc>,
) {
    sqlx::query(
        "INSERT INTO requests \
         (id, runtime_generation_id, api_key_id, route_slug, operation, surface, \
          started_at, completed_at, status_code, total_latency_ms) \
         VALUES ($1, $2, $3, 'partition-route', 'generation', 'openai', \
                 $4, $4, 200, 1)",
    )
    .bind(request_id)
    .bind(generation_id)
    .bind(api_key_id)
    .bind(started_at)
    .execute(pool)
    .await
    .unwrap();
}

async fn request_partition(pool: &sqlx::PgPool, request_id: Uuid) -> String {
    sqlx::query_scalar("SELECT tableoid::regclass::text FROM requests WHERE id = $1")
        .bind(request_id)
        .fetch_one(pool)
        .await
        .unwrap()
}
