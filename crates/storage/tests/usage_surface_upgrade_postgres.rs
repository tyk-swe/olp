use chrono::{Duration, Timelike, Utc};
use olp_storage::{MIGRATOR, PgStore, UsageConsumerState, UsageFilters};
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn pre_0010_usage_surfaces_survive_upgrade_and_rollup() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 2).await.unwrap();
    MIGRATOR.run_to(9, store.pool()).await.unwrap();

    let owner_id = Uuid::now_v7();
    let provider_id = Uuid::now_v7();
    let api_key_id = Uuid::now_v7();
    let generation_id = Uuid::now_v7();
    sqlx::query("INSERT INTO installation (organization_name) VALUES ('Upgrade fixture')")
        .execute(store.pool())
        .await
        .unwrap();
    sqlx::query(
        "INSERT INTO users (id, email, display_name, role) \
         VALUES ($1, 'owner@example.test', 'Owner', 'owner')",
    )
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO providers (id, name, kind, auth_mode, etag, created_by) \
         VALUES ($1, 'upgrade-provider', 'open_ai', 'api_key', $2, $3)",
    )
    .bind(provider_id)
    .bind(Uuid::now_v7())
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO api_keys (id, lookup_id, secret_digest, name, created_by) \
         VALUES ($1, 'olpv2upgrade01', $2, 'upgrade key', $3)",
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
    for (key, value) in [
        ("retention.requests_days", "30"),
        ("retention.usage_days", "90"),
        ("retention.audit_days", "365"),
    ] {
        sqlx::query("INSERT INTO settings (key, value, etag, updated_by) VALUES ($1, $2, $3, $4)")
            .bind(key)
            .bind(value)
            .bind(Uuid::now_v7())
            .bind(owner_id)
            .execute(store.pool())
            .await
            .unwrap();
    }

    let now = Utc::now();
    let observed_at = (now - Duration::days(100))
        .with_minute(10)
        .unwrap()
        .with_second(0)
        .unwrap()
        .with_nanosecond(0)
        .unwrap();
    let bucket = observed_at.with_minute(0).unwrap().with_second(0).unwrap();
    let cold_bucket = bucket - Duration::days(2);
    let anthropic_request_id = Uuid::now_v7();
    let gemini_request_id = Uuid::now_v7();
    let orphan_request_id = Uuid::now_v7();
    let anthropic_started_at = observed_at - Duration::seconds(3);
    let gemini_started_at = observed_at - Duration::seconds(2);
    let orphan_started_at = observed_at - Duration::seconds(1);

    for (request_id, started_at, surface) in [
        (anthropic_request_id, anthropic_started_at, "anthropic"),
        (gemini_request_id, gemini_started_at, "gemini"),
    ] {
        sqlx::query(
            "INSERT INTO requests \
             (id, runtime_generation_id, api_key_id, route_slug, operation, surface, \
              started_at, completed_at, status_code, total_latency_ms) \
             VALUES ($1, $2, $3, 'default', 'generation', $4, $5, $6, 200, 10)",
        )
        .bind(request_id)
        .bind(generation_id)
        .bind(api_key_id)
        .bind(surface)
        .bind(started_at)
        .bind(observed_at)
        .execute(store.pool())
        .await
        .unwrap();
    }

    for (request_id, started_at) in [
        (anthropic_request_id, anthropic_started_at),
        (gemini_request_id, gemini_started_at),
        (orphan_request_id, orphan_started_at),
    ] {
        sqlx::query(
            "INSERT INTO usage_request_anchors (request_id, request_started_at) VALUES ($1, $2)",
        )
        .bind(request_id)
        .bind(started_at)
        .execute(store.pool())
        .await
        .unwrap();
        sqlx::query(
            "INSERT INTO usage_facts \
             (id, request_id, request_started_at, api_key_id, provider_id, route_slug, \
              upstream_model, operation, observed_at, input_tokens, output_tokens, \
              estimated_cost, unpriced, usage_complete) \
             VALUES ($1, $2, $3, $4, $5, 'default', 'legacy-model', 'generation', \
                     $6, 10, 5, 0.100000000000, false, true)",
        )
        .bind(Uuid::now_v7())
        .bind(request_id)
        .bind(started_at)
        .bind(api_key_id)
        .bind(provider_id)
        .bind(observed_at)
        .execute(store.pool())
        .await
        .unwrap();
    }

    // This stale aggregate overlaps retained facts and must be removed before
    // the post-upgrade maintenance pass rebuilds it with exact surfaces.
    sqlx::query(
        "INSERT INTO usage_hourly \
         (bucket, route_slug, provider_id, upstream_model, operation, request_count, \
          input_tokens, output_tokens, estimated_cost, unpriced_count, incomplete_count) \
         VALUES ($1, 'default', $2, 'legacy-model', 'generation', 99, 990, 495, \
                 9.900000000000, 0, 0)",
    )
    .bind(bucket)
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
    // This aggregate has outlived its raw facts. Its historical surface cannot
    // be recovered and therefore must not be relabelled as OpenAI.
    sqlx::query(
        "INSERT INTO usage_hourly \
         (bucket, route_slug, provider_id, upstream_model, operation, request_count, \
          input_tokens, output_tokens, estimated_cost, unpriced_count, incomplete_count) \
         VALUES ($1, 'cold', $2, 'legacy-model', 'generation', 4, 40, 20, \
                 0.400000000000, 0, 0)",
    )
    .bind(cold_bucket)
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();

    MIGRATOR.run(store.pool()).await.unwrap();

    let anthropic_surface: String =
        sqlx::query_scalar("SELECT surface FROM usage_facts WHERE request_id = $1")
            .bind(anthropic_request_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    let gemini_surface: String =
        sqlx::query_scalar("SELECT surface FROM usage_facts WHERE request_id = $1")
            .bind(gemini_request_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    let orphan: (String, bool) =
        sqlx::query_as("SELECT surface, usage_complete FROM usage_facts WHERE request_id = $1")
            .bind(orphan_request_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(anthropic_surface, "anthropic");
    assert_eq!(gemini_surface, "gemini");
    assert_eq!(orphan, ("unknown".to_owned(), false));

    let overlapping_count: i64 =
        sqlx::query_scalar("SELECT count(*) FROM usage_hourly WHERE bucket = $1")
            .bind(bucket)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(overlapping_count, 0);
    let cold: (String, i64) =
        sqlx::query_as("SELECT surface, incomplete_count FROM usage_hourly WHERE bucket = $1")
            .bind(cold_bucket)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(cold, ("unknown".to_owned(), 4));

    sqlx::query(
        "INSERT INTO usage_consumer_health \
         (singleton, pending_events, lag_events, checked_at) VALUES (true, 0, 0, $1)",
    )
    .bind(now)
    .execute(store.pool())
    .await
    .unwrap();
    let report = store.run_maintenance(now).await.unwrap();
    assert_eq!(report.rollup_rows, 3);
    let remaining_facts: i64 = sqlx::query_scalar("SELECT count(*) FROM usage_facts")
        .fetch_one(store.pool())
        .await
        .unwrap();
    assert_eq!(remaining_facts, 0);

    let rows: Vec<(String, i64, i64)> = sqlx::query_as(
        "SELECT surface, sum(request_count)::bigint, sum(incomplete_count)::bigint \
         FROM usage_hourly GROUP BY surface ORDER BY surface",
    )
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert_eq!(
        rows,
        vec![
            ("anthropic".to_owned(), 1, 0),
            ("gemini".to_owned(), 1, 0),
            ("unknown".to_owned(), 5, 5),
        ]
    );

    let filters = UsageFilters {
        observed_after: cold_bucket,
        observed_before: bucket + Duration::hours(1),
        route_slug: None,
        provider_id: None,
        upstream_model: None,
        api_key_id: None,
        operation: None,
    };
    let completeness = store.usage_completeness(&filters).await.unwrap();
    assert_eq!(completeness.request_count, 7);
    assert_eq!(completeness.priced_count, 7);
    assert_eq!(completeness.unpriced_count, 0);
    assert_eq!(completeness.incomplete_count, 5);
    assert_eq!(completeness.ingestion_gap_events, 5);
    assert!(completeness.coverage.range_complete);
    assert_eq!(completeness.consumer.state, UsageConsumerState::Healthy);
    assert!(!completeness.complete);
}
