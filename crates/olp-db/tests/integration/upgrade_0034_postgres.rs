use chrono::{Duration, Timelike, Utc};
use olp_db::{MIGRATOR, usage::Filters};
use rust_decimal::Decimal;
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn upgrade_0034_reconciles_legacy_usage_and_retires_tables() {
    let db = olp_db::test_support::TestDb::create_empty("upgrade_0034").await;
    let store = db.store(2).await;
    MIGRATOR.run_to(33, store.pool()).await.unwrap();

    let owner_id = Uuid::now_v7();
    let provider_id = Uuid::now_v7();
    let second_provider_id = Uuid::now_v7();
    let api_key_id = Uuid::now_v7();
    let generation_id = Uuid::now_v7();

    sqlx::query("INSERT INTO installation (installation_name) VALUES ('Upgrade 0034 fixture')")
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
         VALUES ($1, 'first-provider', 'openai', 'api_key', $2, $3), \
                ($4, 'second-provider', 'openai', 'api_key', $5, $3)",
    )
    .bind(provider_id)
    .bind(Uuid::now_v7())
    .bind(owner_id)
    .bind(second_provider_id)
    .bind(Uuid::now_v7())
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO api_keys (id, lookup_id, secret_digest, name, created_by) \
         VALUES ($1, 'olpv2upgr0034', $2, 'upgrade 0034 key', $3)",
    )
    .bind(api_key_id)
    .bind([8_u8; 32].as_slice())
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

    let revision_id = Uuid::now_v7();
    let pricing_started_at = Utc::now() - Duration::days(30);
    sqlx::query(
        "INSERT INTO pricing_revisions (id, revision, effective_at, created_by) \
         VALUES ($1, 1, $2, $3)",
    )
    .bind(revision_id)
    .bind(pricing_started_at)
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO prices \
         (pricing_revision_id, provider_kind, provider_id, model, operation, \
          input_per_million, output_per_million, currency) \
         VALUES ($1, 'openai', NULL, 'test-model', 'generation', 2000000, 4000000, 'USD')",
    )
    .bind(revision_id)
    .execute(store.pool())
    .await
    .unwrap();

    let now = Utc::now();
    let bucket = now
        .with_minute(0)
        .unwrap()
        .with_second(0)
        .unwrap()
        .with_nanosecond(0)
        .unwrap()
        - Duration::hours(2);

    let request_id = Uuid::now_v7();
    let event_id = Uuid::now_v7();
    let first_attempt_id = Uuid::now_v7();
    let second_attempt_id = Uuid::now_v7();
    let started_at = bucket + Duration::minutes(5);
    let completed_at = started_at + Duration::milliseconds(30);

    sqlx::query(
        "INSERT INTO requests \
         (id, runtime_generation_id, api_key_id, route_slug, operation, surface, \
          started_at, completed_at, status_code, total_latency_ms, first_byte_ms, \
          attempt_count) \
         VALUES ($1, $2, $3, 'upgrade-0034-route', 'generation', 'openai', \
                 $4, $5, 200, 30, 15, 2)",
    )
    .bind(request_id)
    .bind(generation_id)
    .bind(api_key_id)
    .bind(started_at)
    .bind(completed_at)
    .execute(store.pool())
    .await
    .unwrap();

    sqlx::query(
        "INSERT INTO attempts \
         (id, request_id, request_started_at, ordinal, provider_id, upstream_model, \
          started_at, completed_at, status_code, error_class, committed, latency_ms) \
         VALUES \
         ($1, $2, $3, 1, $4, 'test-model', $3, $3 + interval '10 milliseconds', 504, 'timeout', false, 10), \
         ($5, $2, $3, 2, $6, 'test-model', $3 + interval '15 milliseconds', $7, 200, NULL, true, 15)",
    )
    .bind(first_attempt_id)
    .bind(request_id)
    .bind(started_at)
    .bind(provider_id)
    .bind(second_attempt_id)
    .bind(second_provider_id)
    .bind(completed_at)
    .execute(store.pool())
    .await
    .unwrap();

    sqlx::query(
        "INSERT INTO usage_request_anchors (request_id, request_started_at) VALUES ($1, $2)",
    )
    .bind(request_id)
    .bind(started_at)
    .execute(store.pool())
    .await
    .unwrap();

    // Insert legacy usage_fact and legacy usage_hourly
    sqlx::query(
        "INSERT INTO usage_facts \
         (id, request_id, request_started_at, api_key_id, provider_id, route_slug, \
          upstream_model, operation, surface, observed_at, input_tokens, output_tokens, \
          cached_input_tokens, media_units, estimated_cost, unpriced, usage_complete, \
          pricing_revision_id, currency) \
         VALUES ($1, $2, $3, $4, $5, 'upgrade-0034-route', 'test-model', 'generation', \
                 'openai', $6, 100, 50, 10, 0, 0.000400000000, false, true, $7, 'USD')",
    )
    .bind(event_id)
    .bind(request_id)
    .bind(started_at)
    .bind(api_key_id)
    .bind(second_provider_id)
    .bind(completed_at)
    .bind(revision_id)
    .execute(store.pool())
    .await
    .unwrap();

    // Clear attempt_usage_facts and attempt_usage_hourly to test migration 0034 reconciliation logic
    sqlx::query("DELETE FROM attempt_usage_facts WHERE request_id = $1")
        .bind(request_id)
        .execute(store.pool())
        .await
        .unwrap();

    let archived_bucket = bucket - Duration::hours(10);
    sqlx::query("SELECT set_config('olp.attempt_usage_hourly_mirror', 'off', true)")
        .execute(store.pool())
        .await
        .unwrap();
    sqlx::query("SELECT set_config('olp.attempt_usage_legacy_archive', 'off', true)")
        .execute(store.pool())
        .await
        .unwrap();
    sqlx::query("SELECT set_config('olp.usage_rollup_writer', 'additive-v2', true)")
        .execute(store.pool())
        .await
        .unwrap();
    let mut tx = store.pool().begin().await.unwrap();
    sqlx::query("SELECT set_config('olp.usage_rollup_writer', 'additive-v2', true)")
        .execute(&mut *tx)
        .await
        .unwrap();
    sqlx::query(
        "INSERT INTO usage_hourly \
         (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id, \
          request_count, input_tokens, output_tokens, cached_input_tokens, media_units, \
          estimated_cost, unpriced_count, incomplete_count, currency) \
         VALUES ($1, 'upgrade-0034-route', $2, 'test-model', 'generation', 'openai', $3, \
                 5, 500, 250, 50, 0, 0.002000000000, 0, 0, 'USD')",
    )
    .bind(archived_bucket)
    .bind(provider_id)
    .bind(api_key_id)
    .execute(&mut *tx)
    .await
    .unwrap();
    tx.commit().await.unwrap();

    // Now run migration 0034
    MIGRATOR.run(store.pool()).await.unwrap();

    // Assert that legacy tables are dropped
    let legacy_facts_exists: bool = sqlx::query_scalar(
        "SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'usage_facts')",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert!(!legacy_facts_exists, "usage_facts table must be dropped");

    let legacy_hourly_exists: bool = sqlx::query_scalar(
        "SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'usage_hourly')",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert!(!legacy_hourly_exists, "usage_hourly table must be dropped");

    // Assert legacy triggers are dropped
    let legacy_trigger_count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM pg_trigger WHERE tgname IN ( \
           'usage_facts_attempt_mirror', 'usage_hourly_attempt_mirror', \
           'usage_facts_attempt_legacy_archive', 'usage_facts_request_metadata_receipt_guard', \
           'usage_facts_preserve_request_metadata_receipt', 'usage_hourly_writer_guard')",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(
        legacy_trigger_count, 0,
        "all legacy triggers must be dropped"
    );

    // Assert legacy trigger functions are dropped
    let legacy_function_count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM pg_proc WHERE proname IN ( \
           'mirror_legacy_usage_fact_to_attempt', 'mirror_legacy_usage_hourly_to_attempt', \
           'archive_attempt_usage_for_legacy_rollup', 'enforce_request_metadata_fact_receipt', \
           'preserve_request_metadata_fact_receipt', 'enforce_usage_fact_receipt', \
           'preserve_usage_fact_receipt')",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(
        legacy_function_count, 0,
        "all legacy trigger functions must be dropped"
    );

    // Assert attempt_usage_facts contains reconciled facts
    let reconciled_facts: Vec<(i16, Uuid, String, i64, i64, bool, bool)> = sqlx::query_as(
        "SELECT attempt_ordinal, provider_id, charge_status::text, \
                COALESCE(input_tokens, 0), COALESCE(output_tokens, 0), \
                request_counted, provider_request_counted \
         FROM attempt_usage_facts WHERE request_id = $1 ORDER BY attempt_ordinal",
    )
    .bind(request_id)
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert_eq!(reconciled_facts.len(), 2);
    assert_eq!(reconciled_facts[0].0, 1);
    assert_eq!(reconciled_facts[0].1, provider_id);
    assert_eq!(reconciled_facts[0].2, "billing_uncertain");
    assert!(reconciled_facts[0].5); // request_counted (first attempt is request marker)
    assert!(reconciled_facts[0].6); // provider_request_counted

    assert_eq!(reconciled_facts[1].0, 2);
    assert_eq!(reconciled_facts[1].1, second_provider_id);
    assert_eq!(reconciled_facts[1].2, "billable");
    assert_eq!(reconciled_facts[1].3, 100);
    assert_eq!(reconciled_facts[1].4, 50);
    assert!(!reconciled_facts[1].5); // not request_counted (second attempt)
    assert!(reconciled_facts[1].6); // provider_request_counted

    // Assert attempt_usage_hourly contains reconciled hourly rows
    let reconciled_hourly: (i64, i64, Decimal, Decimal, Decimal) = sqlx::query_as(
        "SELECT request_count, provider_request_count, input_tokens, output_tokens, estimated_cost \
         FROM attempt_usage_hourly WHERE bucket = $1 AND route_slug = 'upgrade-0034-route'",
    )
    .bind(archived_bucket)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(reconciled_hourly.0, 5);
    assert_eq!(reconciled_hourly.1, 5);
    assert_eq!(reconciled_hourly.2, Decimal::from(500));
    assert_eq!(reconciled_hourly.3, Decimal::from(250));
    assert_eq!(reconciled_hourly.4, Decimal::new(20, 4));

    // Verify usage query summary
    let filters = Filters {
        observed_after: archived_bucket - Duration::hours(1),
        observed_before: completed_at + Duration::hours(1),
        route_slug: Some("upgrade-0034-route".to_owned()),
        provider_id: None,
        upstream_model: None,
        api_key_id: None,
        operation: None,
    };
    let summary = store.usage_summary(&filters).await.unwrap();
    assert_eq!(summary.request_count, 6); // 5 from hourly + 1 from fact
    assert_eq!(summary.input_tokens, "600"); // 500 + 100
    assert_eq!(summary.output_tokens, "300"); // 250 + 50
}
