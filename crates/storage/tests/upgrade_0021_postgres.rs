use chrono::{Duration, Utc};
use olp_storage::{MIGRATOR, PgStore};
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn schema_0021_data_upgrades_without_bulk_receipts_and_new_writers_are_fenced() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 3).await.unwrap();
    MIGRATOR.run_to(21, store.pool()).await.unwrap();

    let owner_id = Uuid::now_v7();
    sqlx::query("INSERT INTO installation (organization_name) VALUES ('0021 upgrade fixture')")
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
    let provider_id = Uuid::now_v7();
    let api_key_id = Uuid::now_v7();
    let generation_id = Uuid::now_v7();
    let request_id = Uuid::now_v7();
    let fact_id = Uuid::now_v7();
    let provider_model_id = Uuid::now_v7();
    let provider_revision_id = Uuid::now_v7();
    let provider_revision_model_id = Uuid::now_v7();
    let pricing_revision_id = Uuid::now_v7();
    let media_job_id = Uuid::now_v7();
    let draft_id = Uuid::now_v7();
    let draft_target_id = Uuid::now_v7();
    let route_id = Uuid::now_v7();
    let route_revision_id = Uuid::now_v7();
    let revision_target_id = Uuid::now_v7();
    let revision_second_target_id = Uuid::now_v7();
    let restored_draft_id = Uuid::now_v7();
    let restored_matching_target_id = Uuid::now_v7();
    let restored_edited_target_id = Uuid::now_v7();
    let observed_at = Utc::now() - Duration::days(2);

    sqlx::query(
        "INSERT INTO providers \
         (id, name, kind, state, auth_mode, etag, created_by) \
         VALUES ($1, 'upgrade-provider', 'open_ai', 'draft', 'api_key', $2, $3)",
    )
    .bind(provider_id)
    .bind(Uuid::now_v7())
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    // This fixture is intentionally written at schema 0021, before 0027 adds
    // routing identities. The upgrade must preserve its existing live scores.
    sqlx::query(
        "INSERT INTO provider_models \
         (id, provider_id, upstream_model, display_name, enabled) \
         VALUES ($1, $2, 'upgrade-model', 'Upgrade model', true)",
    )
    .bind(provider_model_id)
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO route_drafts \
         (id, slug, state, overall_timeout_ms, max_attempts, etag, created_by) \
         VALUES ($1, 'upgrade-route', 'validated', 30000, 1, $2, $3)",
    )
    .bind(draft_id)
    .bind(Uuid::now_v7())
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO route_draft_targets \
         (id, route_draft_id, provider_model_id, priority, weight, timeout_ms, position) \
         VALUES ($1, $2, $3, 0, 1, 20000, 0)",
    )
    .bind(draft_target_id)
    .bind(draft_id)
    .bind(provider_model_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query("INSERT INTO routes (id, slug, created_by) VALUES ($1, 'upgrade-route', $2)")
        .bind(route_id)
        .bind(owner_id)
        .execute(store.pool())
        .await
        .unwrap();
    sqlx::query(
        "INSERT INTO route_revisions \
         (id, route_id, revision, slug, overall_timeout_ms, max_attempts, source_draft_id, activated_by) \
         VALUES ($1, $2, 1, 'upgrade-route', 30000, 1, $3, $4)",
    )
    .bind(route_revision_id)
    .bind(route_id)
    .bind(draft_id)
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO route_revision_targets \
         (id, route_revision_id, provider_model_id, priority, weight, timeout_ms, position) \
         VALUES ($1, $2, $3, 0, 1, 20000, 0)",
    )
    .bind(revision_target_id)
    .bind(route_revision_id)
    .bind(provider_model_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO route_revision_targets \
         (id, route_revision_id, provider_model_id, priority, weight, timeout_ms, position) \
         VALUES ($1, $2, $3, 0, 2, 20000, 1)",
    )
    .bind(revision_second_target_id)
    .bind(route_revision_id)
    .bind(provider_model_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO route_drafts \
         (id, slug, state, overall_timeout_ms, max_attempts, etag, based_on_revision_id, created_by) \
         VALUES ($1, 'upgrade-route', 'draft', 30000, 1, $2, $3, $4)",
    )
    .bind(restored_draft_id)
    .bind(Uuid::now_v7())
    .bind(route_revision_id)
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO route_draft_targets \
         (id, route_draft_id, provider_model_id, priority, weight, timeout_ms, position) \
         VALUES ($1, $2, $3, 0, 1, 20000, 0)",
    )
    .bind(restored_matching_target_id)
    .bind(restored_draft_id)
    .bind(provider_model_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO route_draft_targets \
         (id, route_draft_id, provider_model_id, priority, weight, timeout_ms, position) \
         VALUES ($1, $2, $3, 0, 3, 20000, 1)",
    )
    .bind(restored_edited_target_id)
    .bind(restored_draft_id)
    .bind(provider_model_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO api_keys (id, lookup_id, secret_digest, name, created_by) \
         VALUES ($1, 'olpv2upgrade21', $2, 'upgrade key', $3)",
    )
    .bind(api_key_id)
    .bind([7_u8; 32].as_slice())
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO runtime_generations \
         (id, compiled_release, release_sha256, created_by) VALUES ($1, $2, $3, $4)",
    )
    .bind(generation_id)
    .bind([1_u8].as_slice())
    .bind([2_u8; 32].as_slice())
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO model_capabilities \
         (provider_model_id, operation, surface, mode, source) \
         VALUES ($1, 'generation', 'open_ai', 'unary', 'declared')",
    )
    .bind(provider_model_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO provider_revisions \
         (id, provider_id, revision, name, kind, auth_mode, connector_ready, \
          source_etag, activated_by) \
         SELECT $1, id, 1, name, kind, auth_mode, true, etag, $2 \
         FROM providers WHERE id = $3",
    )
    .bind(provider_revision_id)
    .bind(owner_id)
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO provider_revision_models \
         (id, provider_revision_id, source_provider_model_id, upstream_model, \
          display_name, enabled) \
         VALUES ($1, $2, $3, 'upgrade-model', 'Upgrade model', true)",
    )
    .bind(provider_revision_model_id)
    .bind(provider_revision_id)
    .bind(provider_model_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO provider_revision_capabilities \
         (provider_revision_model_id, operation, surface, mode, source) \
         VALUES ($1, 'generation', 'open_ai', 'unary', 'declared')",
    )
    .bind(provider_revision_model_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO runtime_generation_provider_configs \
         (runtime_generation_id, provider_id, kind, auth_mode, provider_revision_id) \
         VALUES ($1, $2, 'open_ai', 'api_key', $3)",
    )
    .bind(generation_id)
    .bind(provider_id)
    .bind(provider_revision_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO pricing_revisions (id, revision, effective_at, created_by) \
         VALUES ($1, 1, now(), $2)",
    )
    .bind(pricing_revision_id)
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO prices \
         (pricing_revision_id, provider_kind, model, operation, input_per_million) \
         VALUES ($1, 'open_ai', 'upgrade-model', 'generation', 1)",
    )
    .bind(pricing_revision_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO async_media_jobs \
         (id, upstream_job_id, api_key_id, provider_id, route_slug, operation, state, \
          provider_model, surface) \
         VALUES ($1, 'legacy-job', $2, $3, 'upgrade', 'video_create', 'running', \
                 'upgrade-model', 'openai')",
    )
    .bind(media_job_id)
    .bind(api_key_id)
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO requests \
         (id, runtime_generation_id, api_key_id, route_slug, operation, surface, \
          started_at, completed_at, status_code, total_latency_ms, attempt_count) \
         VALUES ($1, $2, $3, 'upgrade', 'generation', 'open_ai', $4, $5, 200, 10, 1)",
    )
    .bind(request_id)
    .bind(generation_id)
    .bind(api_key_id)
    .bind(observed_at - Duration::milliseconds(10))
    .bind(observed_at)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO usage_request_anchors (request_id, request_started_at) VALUES ($1, $2)",
    )
    .bind(request_id)
    .bind(observed_at - Duration::milliseconds(10))
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO usage_facts \
         (id, request_id, request_started_at, api_key_id, provider_id, route_slug, \
          upstream_model, operation, surface, observed_at, input_tokens, output_tokens, \
          unpriced, usage_complete) \
         VALUES ($1, $2, $3, $4, $5, 'upgrade', 'model', 'generation', 'open_ai', \
                 $6, 3, 2, true, true)",
    )
    .bind(fact_id)
    .bind(request_id)
    .bind(observed_at - Duration::milliseconds(10))
    .bind(api_key_id)
    .bind(provider_id)
    .bind(observed_at)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO usage_hourly \
         (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id, \
          request_count, input_tokens, output_tokens, cached_input_tokens, media_units, \
          unpriced_count, incomplete_count) \
         VALUES (date_trunc('hour', $1::timestamptz), 'retained', $2, 'model', \
                 'generation', 'open_ai', $3, 4, 12, 8, 0, 0, 4, 0)",
    )
    .bind(observed_at - Duration::days(10))
    .bind(provider_id)
    .bind(api_key_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO usage_hourly \
         (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id, \
          request_count, input_tokens, output_tokens, cached_input_tokens, media_units, \
          unpriced_count, incomplete_count) \
         VALUES (date_trunc('hour', $1::timestamptz), 'unknown-retained', $2, 'model', \
                 'generation', 'unknown', $3, 1, 0, 0, 0, 0, 1, 1)",
    )
    .bind(observed_at - Duration::days(11))
    .bind(provider_id)
    .bind(api_key_id)
    .execute(store.pool())
    .await
    .unwrap();

    let configuration_id = Uuid::now_v7();
    let configuration_etag = Uuid::now_v7();
    let flow_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO oidc_configurations \
         (id, issuer, client_id, enabled, singleton, etag, updated_by) \
         VALUES ($1, 'https://idp.example.test', 'upgrade-client', false, true, $2, $3)",
    )
    .bind(configuration_id)
    .bind(configuration_etag)
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO oidc_authorization_flows \
         (id, configuration_id, purpose, actor_user_id, state_digest, browser_binding_digest, \
          client_digest, encrypted_payload, payload_nonce, payload_key_version, expires_at) \
         VALUES ($1, $2, 'link', $3, $4, $5, NULL, $6, $7, 1, now() + interval '5 minutes')",
    )
    .bind(flow_id)
    .bind(configuration_id)
    .bind(owner_id)
    .bind([3_u8; 32].as_slice())
    .bind([4_u8; 32].as_slice())
    .bind([5_u8; 16].as_slice())
    .bind([6_u8; 12].as_slice())
    .execute(store.pool())
    .await
    .unwrap();

    MIGRATOR.run_to(24, store.pool()).await.unwrap();

    let migrated_etag: Uuid =
        sqlx::query_scalar("SELECT configuration_etag FROM oidc_authorization_flows WHERE id = $1")
            .bind(flow_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(migrated_etag, configuration_etag);

    MIGRATOR.run(store.pool()).await.unwrap();
    let invalidated_flow_count: i64 =
        sqlx::query_scalar("SELECT count(*) FROM oidc_authorization_flows WHERE id = $1")
            .bind(flow_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(invalidated_flow_count, 0);

    let renamed_dimensions: Vec<(String, String)> = sqlx::query_as(
        "SELECT 'providers', kind FROM providers \
         UNION ALL SELECT 'model_capabilities', surface FROM model_capabilities \
         UNION ALL SELECT 'prices', provider_kind FROM prices \
         UNION ALL SELECT 'requests', surface FROM requests \
         UNION ALL SELECT 'usage_facts', surface FROM usage_facts \
         UNION ALL SELECT 'usage_hourly', surface FROM usage_hourly WHERE surface <> 'unknown' \
         UNION ALL SELECT 'async_media_jobs', surface FROM async_media_jobs \
         UNION ALL SELECT 'provider_revisions', kind FROM provider_revisions \
         UNION ALL SELECT 'provider_revision_capabilities', surface \
           FROM provider_revision_capabilities \
         UNION ALL SELECT 'runtime_generation_provider_configs', kind \
           FROM runtime_generation_provider_configs",
    )
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert_eq!(renamed_dimensions.len(), 10);
    for (dimension, value) in renamed_dimensions {
        assert_eq!(value, "openai", "{dimension} retained a legacy value");
    }
    let unknown_surfaces: i64 =
        sqlx::query_scalar("SELECT count(*) FROM usage_hourly WHERE surface = 'unknown'")
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(unknown_surfaces, 1);

    let release_envelope: (Vec<u8>, Vec<u8>) = sqlx::query_as(
        "SELECT compiled_release, release_sha256 FROM runtime_generations WHERE id = $1",
    )
    .bind(generation_id)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(release_envelope, (vec![1], vec![2; 32]));

    let naming_constraints: Vec<String> = sqlx::query_scalar(
        "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname IN ( \
           'prices_provider_kind_check', 'usage_facts_surface_check', \
           'usage_hourly_surface_check', 'async_media_jobs_surface_check') \
         ORDER BY conname",
    )
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert_eq!(naming_constraints.len(), 4);
    assert!(naming_constraints.iter().all(|definition| {
        definition.contains("'openai'") && !definition.contains("'open_ai'")
    }));

    let draft_routing_id: Uuid =
        sqlx::query_scalar("SELECT routing_id FROM route_drafts WHERE id = $1")
            .bind(draft_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    let draft_target_routing_id: Uuid =
        sqlx::query_scalar("SELECT routing_id FROM route_draft_targets WHERE id = $1")
            .bind(draft_target_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    let revision_routing_id: Uuid =
        sqlx::query_scalar("SELECT routing_id FROM route_revisions WHERE id = $1")
            .bind(route_revision_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    let revision_target_routing_id: Uuid =
        sqlx::query_scalar("SELECT routing_id FROM route_revision_targets WHERE id = $1")
            .bind(revision_target_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    let restored_draft_routing_id: Uuid =
        sqlx::query_scalar("SELECT routing_id FROM route_drafts WHERE id = $1")
            .bind(restored_draft_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    let restored_matching_target_routing_id: Uuid =
        sqlx::query_scalar("SELECT routing_id FROM route_draft_targets WHERE id = $1")
            .bind(restored_matching_target_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    let restored_edited_target_routing_id: Uuid =
        sqlx::query_scalar("SELECT routing_id FROM route_draft_targets WHERE id = $1")
            .bind(restored_edited_target_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(draft_routing_id, route_id);
    assert_eq!(draft_target_routing_id, revision_target_id);
    assert_eq!(revision_routing_id, route_id);
    assert_eq!(revision_target_routing_id, revision_target_id);
    assert_eq!(restored_draft_routing_id, route_id);
    assert_eq!(restored_matching_target_routing_id, revision_target_id);
    assert_eq!(restored_edited_target_routing_id, restored_edited_target_id);
    let eager_receipts: i64 =
        sqlx::query_scalar("SELECT count(*) FROM request_metadata_event_receipts")
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(eager_receipts, 0, "migration must not bulk-copy raw facts");

    sqlx::query("DELETE FROM usage_facts WHERE id = $1")
        .bind(fact_id)
        .execute(store.pool())
        .await
        .unwrap();
    let preserved_status: String = sqlx::query_scalar(
        "SELECT status::text FROM request_metadata_event_receipts WHERE event_id = $1",
    )
    .bind(fact_id)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(preserved_status, "fact_persisted");

    let legacy_rollup_error =
        sqlx::query("UPDATE usage_hourly SET request_count = 1 WHERE route_slug = 'retained'")
            .execute(store.pool())
            .await
            .unwrap_err();
    assert_eq!(
        legacy_rollup_error
            .as_database_error()
            .and_then(sqlx::error::DatabaseError::code)
            .as_deref(),
        Some("55000")
    );
    let empty_legacy_rollup_error =
        sqlx::query("UPDATE usage_hourly SET request_count = 1 WHERE false")
            .execute(store.pool())
            .await
            .unwrap_err();
    assert_eq!(
        empty_legacy_rollup_error
            .as_database_error()
            .and_then(sqlx::error::DatabaseError::code)
            .as_deref(),
        Some("55000")
    );
    let empty_legacy_gap_rollup_error =
        sqlx::query("UPDATE request_metadata_gap_hourly SET event_count = event_count WHERE false")
            .execute(store.pool())
            .await
            .unwrap_err();
    assert_eq!(
        empty_legacy_gap_rollup_error
            .as_database_error()
            .and_then(sqlx::error::DatabaseError::code)
            .as_deref(),
        Some("55000")
    );

    let mut legacy_runtime = store
        .pool()
        .begin_with("BEGIN ISOLATION LEVEL REPEATABLE READ")
        .await
        .unwrap();
    let legacy_runtime_error = sqlx::query(
        "INSERT INTO runtime_generations \
         (id, compiled_release, release_sha256, created_by) VALUES ($1, $2, $3, $4)",
    )
    .bind(Uuid::now_v7())
    .bind([8_u8].as_slice())
    .bind([9_u8; 32].as_slice())
    .bind(owner_id)
    .execute(&mut *legacy_runtime)
    .await
    .unwrap_err();
    assert_eq!(
        legacy_runtime_error
            .as_database_error()
            .and_then(sqlx::error::DatabaseError::code)
            .as_deref(),
        Some("55000")
    );
    legacy_runtime.rollback().await.unwrap();
}
