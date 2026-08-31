//! Regression coverage for the persistence-layer correctness fixes: immutable
//! history that survives provider changes, activation guards that name what
//! they reject, UTC hour buckets, media-job reconciliation state, public-auth
//! rate-limit accounting, the invitation lifecycle, release verification, and
//! database-clock worker ages.

use chrono::{DateTime, Duration, Timelike as _, Utc};
use olp_db::{
    configuration::Error as ConfigurationError,
    idempotency::{Outcome, Replayable, Response},
    identity::{AcceptInvitation, Error as IdentityError, InstallationSetupInput, NewInvitation},
    media_jobs::{MediaJobState, MediaJobUpdate, NewMediaJobReservation},
    security::{envelope::MasterKey, password::hash, session_material::SessionMaterial},
    store::Store,
    usage::{Filters, Granularity},
    worker_health::{WorkerTask, WorkerTaskCheckpointOutcome, WorkerTaskState},
};
use olp_engine::domain::auth::Role;
use sqlx::{Connection as _, PgConnection, PgPool, postgres::PgPoolOptions};
use uuid::Uuid;

const ROUTE_OPERATION: &str = "generation";

async fn owner_id(store: &Store, label: &str) -> Uuid {
    let (owner, _) = store
        .setup_installation_with_session(
            InstallationSetupInput {
                installation_name: format!("Persistence {label}"),
                email: format!("owner@{label}.test"),
                display_name: "Owner".to_owned(),
                password_hash: "test-password-hash".to_owned(),
            },
            &SessionMaterial::generate(),
            Duration::hours(1),
        )
        .await
        .unwrap();
    owner.user_id
}

/// Inserts a two-model OpenAI provider with a matching activated revision.
async fn insert_two_model_provider(
    pool: &PgPool,
    actor: Uuid,
    name: &str,
) -> (Uuid, Uuid, Uuid, Uuid) {
    let provider_id = Uuid::now_v7();
    let etag = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers \
         (id, name, kind, state, endpoint, auth_mode, connector_ready, etag, created_by, \
          last_probe_at, last_probe_status, last_probe_detail) \
         VALUES ($1, $2, 'openai', 'active', 'https://api.example.test/v1/', 'adc', true, $3, $4, \
                 now(), 'succeeded', 'mock probe succeeded')",
    )
    .bind(provider_id)
    .bind(name)
    .bind(etag)
    .bind(actor)
    .execute(pool)
    .await
    .unwrap();
    let primary = insert_model(pool, provider_id, &format!("{name}-primary")).await;
    let secondary = insert_model(pool, provider_id, &format!("{name}-secondary")).await;
    let revision_id =
        insert_provider_revision(pool, actor, provider_id, 1, &[primary, secondary]).await;
    sqlx::query("UPDATE providers SET active_revision_id = $1 WHERE id = $2")
        .bind(revision_id)
        .bind(provider_id)
        .execute(pool)
        .await
        .unwrap();
    (provider_id, primary, secondary, etag)
}

async fn insert_model(pool: &PgPool, provider_id: Uuid, upstream_model: &str) -> Uuid {
    let model_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO provider_models \
         (id, provider_id, upstream_model, display_name, enabled, discovered_at) \
         VALUES ($1, $2, $3, $3, true, now())",
    )
    .bind(model_id)
    .bind(provider_id)
    .bind(upstream_model)
    .execute(pool)
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO model_capabilities \
         (provider_model_id, operation, surface, mode, source, certified_at) \
         VALUES ($1, $2, 'openai', 'unary', 'certified', now()), \
                ($1, $2, 'openai', 'streaming', 'certified', now())",
    )
    .bind(model_id)
    .bind(ROUTE_OPERATION)
    .execute(pool)
    .await
    .unwrap();
    model_id
}

async fn insert_provider_revision(
    pool: &PgPool,
    actor: Uuid,
    provider_id: Uuid,
    revision: i32,
    models: &[Uuid],
) -> Uuid {
    let revision_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO provider_revisions \
         (id, provider_id, revision, name, kind, endpoint, auth_mode, connector_ready, \
          source_etag, activated_by) \
         SELECT $1, p.id, $2, p.name, p.kind, p.endpoint, p.auth_mode, true, p.etag, $3 \
         FROM providers p WHERE p.id = $4",
    )
    .bind(revision_id)
    .bind(revision)
    .bind(actor)
    .bind(provider_id)
    .execute(pool)
    .await
    .unwrap();
    for model_id in models {
        let revision_model_id = Uuid::now_v7();
        sqlx::query(
            "INSERT INTO provider_revision_models \
             (id, provider_revision_id, source_provider_model_id, upstream_model, \
              display_name, enabled, discovered_at) \
             SELECT $1, $2, pm.id, pm.upstream_model, pm.display_name, pm.enabled, now() \
             FROM provider_models pm WHERE pm.id = $3",
        )
        .bind(revision_model_id)
        .bind(revision_id)
        .bind(model_id)
        .execute(pool)
        .await
        .unwrap();
        sqlx::query(
            "INSERT INTO provider_revision_capabilities \
             (provider_revision_model_id, operation, surface, mode, source, certified_at) \
             VALUES ($1, $2, 'openai', 'unary', 'certified', now()), \
                    ($1, $2, 'openai', 'streaming', 'certified', now())",
        )
        .bind(revision_model_id)
        .bind(ROUTE_OPERATION)
        .execute(pool)
        .await
        .unwrap();
    }
    revision_id
}

/// Inserts one active route revision plus a draft, both targeting `models`.
async fn insert_route(
    pool: &PgPool,
    actor: Uuid,
    slug: &str,
    models: &[Uuid],
) -> (Uuid, Uuid, Uuid) {
    let draft_id = Uuid::now_v7();
    let route_id = Uuid::now_v7();
    let revision_id = Uuid::now_v7();
    let max_attempts = i16::try_from(models.len()).unwrap();
    sqlx::query(
        "INSERT INTO route_drafts \
         (id, routing_id, slug, state, overall_timeout_ms, max_attempts, etag, created_by) \
         VALUES ($1, $2, $3, 'validated', 30000, $4, $5, $6)",
    )
    .bind(draft_id)
    .bind(Uuid::now_v7())
    .bind(slug)
    .bind(max_attempts)
    .bind(Uuid::now_v7())
    .bind(actor)
    .execute(pool)
    .await
    .unwrap();
    sqlx::query("INSERT INTO routes (id, slug, created_by) VALUES ($1, $2, $3)")
        .bind(route_id)
        .bind(slug)
        .bind(actor)
        .execute(pool)
        .await
        .unwrap();
    sqlx::query(
        "INSERT INTO route_revisions \
         (id, route_id, routing_id, revision, slug, overall_timeout_ms, max_attempts, \
          source_draft_id, activated_by) \
         VALUES ($1, $2, $3, 1, $4, 30000, $5, $6, $7)",
    )
    .bind(revision_id)
    .bind(route_id)
    .bind(Uuid::now_v7())
    .bind(slug)
    .bind(max_attempts)
    .bind(draft_id)
    .bind(actor)
    .execute(pool)
    .await
    .unwrap();
    sqlx::query("INSERT INTO route_draft_operations (route_draft_id, operation) VALUES ($1, $2)")
        .bind(draft_id)
        .bind(ROUTE_OPERATION)
        .execute(pool)
        .await
        .unwrap();
    sqlx::query(
        "INSERT INTO route_revision_operations (route_revision_id, operation) VALUES ($1, $2)",
    )
    .bind(revision_id)
    .bind(ROUTE_OPERATION)
    .execute(pool)
    .await
    .unwrap();
    for (position, model_id) in models.iter().enumerate() {
        let position = i32::try_from(position).unwrap();
        sqlx::query(
            "INSERT INTO route_draft_targets \
             (id, routing_id, route_draft_id, provider_model_id, priority, weight, timeout_ms, \
              position) VALUES ($1, $2, $3, $4, 0, 1, 20000, $5)",
        )
        .bind(Uuid::now_v7())
        .bind(Uuid::now_v7())
        .bind(draft_id)
        .bind(model_id)
        .bind(position)
        .execute(pool)
        .await
        .unwrap();
        sqlx::query(
            "INSERT INTO route_revision_targets \
             (id, routing_id, route_revision_id, provider_model_id, priority, weight, timeout_ms, \
              position) VALUES ($1, $2, $3, $4, 0, 1, 20000, $5)",
        )
        .bind(Uuid::now_v7())
        .bind(Uuid::now_v7())
        .bind(revision_id)
        .bind(model_id)
        .bind(position)
        .execute(pool)
        .await
        .unwrap();
    }
    (route_id, revision_id, draft_id)
}

// E1 + E11: a revision is immutable history and a draft is written back
// verbatim by the console. Neither read may drop a target because the
// provider's *current* revision no longer carries its model.
#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn route_targets_survive_a_provider_revision_that_dropped_their_model() {
    let db = olp_db::test_support::TestDb::create_migrated("route_history").await;
    let store = db.store(5).await;
    let actor = owner_id(&store, "route-history").await;
    let (provider_id, primary, secondary, _) =
        insert_two_model_provider(store.pool(), actor, "history").await;
    let (route_id, revision_id, draft_id) =
        insert_route(store.pool(), actor, "history", &[primary, secondary]).await;

    let before = store
        .get_route_revision(route_id, revision_id)
        .await
        .unwrap();
    assert_eq!(before.targets.len(), 2);
    assert!(before.targets.iter().all(|target| target.available));

    // A newer activated revision keeps only the primary model.
    let narrowed = insert_provider_revision(store.pool(), actor, provider_id, 2, &[primary]).await;
    sqlx::query("UPDATE providers SET active_revision_id = $1 WHERE id = $2")
        .bind(narrowed)
        .bind(provider_id)
        .execute(store.pool())
        .await
        .unwrap();

    let after = store
        .get_route_revision(route_id, revision_id)
        .await
        .unwrap();
    assert_eq!(
        after.targets.len(),
        2,
        "frozen revision history must not shrink when a provider revision drops a model"
    );
    let dropped = after
        .targets
        .iter()
        .find(|target| target.provider_model_id == secondary)
        .expect("the target whose model left the activated revision is still part of history");
    assert!(!dropped.available);
    assert_eq!(dropped.upstream_model, "history-secondary");
    assert!(
        after
            .targets
            .iter()
            .find(|target| target.provider_model_id == primary)
            .unwrap()
            .available
    );

    let draft = store.get_route_draft(draft_id).await.unwrap();
    assert_eq!(
        draft.targets.len(),
        2,
        "a draft read feeds a full-list write back; a hidden target would be deleted"
    );

    // E11: disabling the provider clears active_revision_id entirely. History
    // still has to survive that, or every past revision reads as empty.
    sqlx::query(
        "UPDATE providers SET state = 'disabled'::provider_state, active_revision_id = NULL \
         WHERE id = $1",
    )
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
    let disabled = store
        .get_route_revision(route_id, revision_id)
        .await
        .unwrap();
    assert_eq!(disabled.targets.len(), 2);
    assert!(disabled.targets.iter().all(|target| !target.available));
    assert_eq!(
        store.get_route_draft(draft_id).await.unwrap().targets.len(),
        2
    );
}

// E2: activation checked operation coverage, not per-target survival, so it
// could orphan a live route target and only fail later with an opaque
// "stored runtime configuration is invalid".
#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn activating_a_provider_that_orphans_a_live_route_target_names_it() {
    let db = olp_db::test_support::TestDb::create_migrated("orphan_target").await;
    let store = db.store(5).await;
    let actor = owner_id(&store, "orphan-target").await;
    let (provider_id, primary, secondary, etag) =
        insert_two_model_provider(store.pool(), actor, "orphan").await;
    insert_route(store.pool(), actor, "orphan", &[primary, secondary]).await;

    // The operator disables the secondary model and re-activates. The primary
    // still covers every route operation, so the coverage guard is satisfied.
    sqlx::query("UPDATE provider_models SET enabled = false WHERE id = $1")
        .bind(secondary)
        .execute(store.pool())
        .await
        .unwrap();
    sqlx::query(
        "UPDATE providers SET state = 'draft'::provider_state, updated_at = last_probe_at \
         WHERE id = $1",
    )
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();

    let error = store
        .activate_provider(provider_id, etag, actor, "orphan-activate-01")
        .await
        .unwrap_err();
    let ConfigurationError::Invalid(detail) = &error else {
        panic!("activation must fail with a named target, got {error:?}");
    };
    assert!(detail.contains("orphan"), "{detail}");
    assert!(detail.contains("orphan-secondary"), "{detail}");

    // Re-enabling it makes the same activation succeed.
    sqlx::query("UPDATE provider_models SET enabled = true WHERE id = $1")
        .bind(secondary)
        .execute(store.pool())
        .await
        .unwrap();
    store
        .activate_provider(provider_id, etag, actor, "orphan-activate-02")
        .await
        .unwrap();
}

// E3: hour buckets are UTC hours, but `date_trunc` on a timestamptz truncates
// in the session TimeZone. Pool connections pin UTC and the SQL names UTC
// explicitly, so neither the database default nor a session override can move
// a bucket off the hour the readers compute.
#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn usage_hour_buckets_stay_utc_under_a_half_hour_session_timezone() {
    let db = olp_db::test_support::TestDb::create_migrated("usage_tz").await;
    {
        let mut connection = PgConnection::connect(db.url()).await.unwrap();
        sqlx::raw_sql(
            "DO $$ BEGIN EXECUTE format('ALTER DATABASE %I SET TimeZone = %L', \
             current_database(), 'Asia/Kolkata'); END $$;",
        )
        .execute(&mut connection)
        .await
        .unwrap();
        connection.close().await.unwrap();
    }

    let store = db.store(4).await;
    let zone: String = sqlx::query_scalar("SHOW TimeZone")
        .fetch_one(store.pool())
        .await
        .unwrap();
    assert_eq!(
        zone, "UTC",
        "pooled connections must pin the session TimeZone"
    );

    // A second pool deliberately keeps the local zone. The rollup SQL has to
    // produce identical buckets through it, or a deployment that ever loses
    // the pin silently writes unreadable aggregates.
    let skewed = Store::from_pool(
        PgPoolOptions::new()
            .max_connections(2)
            .after_connect(|connection, _| {
                Box::pin(async move {
                    sqlx::query("SET TimeZone = 'Asia/Kolkata'")
                        .execute(connection)
                        .await?;
                    Ok(())
                })
            })
            .connect(db.url())
            .await
            .unwrap(),
    );
    let skewed_zone: String = sqlx::query_scalar("SHOW TimeZone")
        .fetch_one(skewed.pool())
        .await
        .unwrap();
    assert_eq!(skewed_zone, "Asia/Kolkata");

    let actor = owner_id(&store, "usage-tz").await;
    let provider_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers (id, name, kind, state, auth_mode, etag, created_by) \
         VALUES ($1, 'usage-tz-provider', 'openai', 'active', 'api_key', $2, $3)",
    )
    .bind(provider_id)
    .bind(Uuid::now_v7())
    .bind(actor)
    .execute(store.pool())
    .await
    .unwrap();
    let api_key_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO api_keys (id, lookup_id, secret_digest, name, created_by) \
         VALUES ($1, 'olpv2usagetz', $2, 'usage tz', $3)",
    )
    .bind(api_key_id)
    .bind([3_u8; 32].as_slice())
    .bind(actor)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO settings (key, value, etag, updated_by) \
         VALUES ('retention.usage_days', '1', $1, $2) \
         ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value",
    )
    .bind(Uuid::now_v7())
    .bind(actor)
    .execute(store.pool())
    .await
    .unwrap();

    // 45 minutes past the hour: a half-hour-offset session would truncate this
    // to HH:30 UTC instead of HH:00 UTC.
    let observed_at = (Utc::now() - Duration::days(3))
        .with_minute(45)
        .and_then(|value| value.with_second(0))
        .and_then(|value| value.with_nanosecond(0))
        .unwrap();
    let request_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO usage_request_anchors (request_id, request_started_at) VALUES ($1, $2)",
    )
    .bind(request_id)
    .bind(observed_at)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO attempt_usage_facts \
         (attempt_id, event_id, request_id, request_started_at, attempt_ordinal, api_key_id, \
          provider_id, route_slug, upstream_model, operation, surface, \
          observed_at, charge_status, usage_observed, usage_complete, \
          input_tokens, output_tokens, cached_input_tokens, unpriced, request_counted, \
          provider_request_counted, model_request_counted, target_request_counted, \
          request_unpriced_counted, provider_unpriced_counted, model_unpriced_counted, \
          target_unpriced_counted, request_incomplete_counted, provider_incomplete_counted, \
          model_incomplete_counted, target_incomplete_counted) \
         VALUES ($1, $2, $3, $4, 1, $5, $6, 'usage-tz', 'mock-model', 'generation', 'openai', \
                 $4, 'billable', true, true, 10, 5, 0, true, \
                 true, true, true, true, true, true, true, true, false, false, false, false)",
    )
    .bind(Uuid::now_v7())
    .bind(Uuid::now_v7())
    .bind(request_id)
    .bind(observed_at)
    .bind(api_key_id)
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();

    let report = skewed.run_maintenance(Utc::now()).await.unwrap();
    assert_eq!(report.usage_rows, 1);
    assert_eq!(report.rollup_rows, 1);

    let bucket: DateTime<Utc> = sqlx::query_scalar("SELECT bucket FROM attempt_usage_hourly")
        .fetch_one(store.pool())
        .await
        .unwrap();
    let expected_bucket = observed_at.with_minute(0).unwrap();
    assert_eq!(
        bucket, expected_bucket,
        "the rollup must bucket on the UTC hour whatever the writing session's TimeZone"
    );

    // The retained bucket now has to be visible to the boundary-coverage
    // query, which derives its candidate buckets in UTC. A mismatched bucket
    // silently reported `range_complete: true` while excluding the data.
    let coverage = store
        .usage_series(
            &Filters {
                observed_after: expected_bucket + Duration::minutes(30),
                observed_before: expected_bucket + Duration::minutes(90),
                route_slug: None,
                provider_id: None,
                upstream_model: None,
                api_key_id: None,
                operation: None,
            },
            Granularity::Hour,
        )
        .await
        .unwrap()
        .coverage;
    assert!(!coverage.range_complete);
    assert!(coverage.approximate);
    assert_eq!(coverage.excluded_partial_aggregate_boundaries, 1);

    // Migration 0035's constraint is stated in UTC too: a local-hour bucket is
    // rejected even when the writing session sits on a half-hour offset.
    let mut transaction = skewed.pool().begin().await.unwrap();
    sqlx::query("SELECT set_config('olp.usage_rollup_writer', 'additive-v2', true)")
        .execute(&mut *transaction)
        .await
        .unwrap();
    let rejected = sqlx::query(
        "INSERT INTO request_metadata_gap_hourly \
         (bucket, gateway_instance, reason, event_count, first_observed_at, last_observed_at) \
         VALUES (date_trunc('hour', $1::timestamptz), 'tz', 'missing_stream_event', 1, $1, $1)",
    )
    .bind(observed_at)
    .execute(&mut *transaction)
    .await;
    assert!(
        rejected.is_err(),
        "a session-local hour bucket must not satisfy the UTC bucket constraint"
    );
    transaction.rollback().await.unwrap();
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn concurrent_route_target_index_migrations_retry_leftover_relations() {
    let db = olp_db::test_support::TestDb::create_empty("index_retry").await;
    let store = db.store(2).await;
    store.migrate_to(45).await.unwrap();

    sqlx::query(
        "CREATE INDEX route_draft_targets_provider_model_idx \
         ON route_draft_targets(provider_model_id)",
    )
    .execute(store.pool())
    .await
    .unwrap();
    store.migrate_to(46).await.unwrap();

    sqlx::query(
        "CREATE INDEX route_revision_targets_provider_model_idx \
         ON route_revision_targets(provider_model_id)",
    )
    .execute(store.pool())
    .await
    .unwrap();
    store.migrate_to(47).await.unwrap();

    let indexes: Vec<(String, bool)> = sqlx::query_as(
        "SELECT indexrelid::regclass::text, indisvalid FROM pg_index \
         WHERE indexrelid IN ( \
           'route_draft_targets_provider_model_idx'::regclass, \
           'route_revision_targets_provider_model_idx'::regclass) \
         ORDER BY indexrelid::regclass::text",
    )
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert_eq!(
        indexes,
        [
            ("route_draft_targets_provider_model_idx".to_owned(), true),
            ("route_revision_targets_provider_model_idx".to_owned(), true)
        ]
    );
}

// E4: staleness must use the same poll gate the claim query uses, and a client
// poll must carry the next reconciliation deadline past that gate.
// E5: a clean reconciliation resets the consecutive-failure counter the
// consumer uses for exponential backoff.
#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn a_polled_media_job_is_neither_stale_nor_backed_off() {
    let db = olp_db::test_support::TestDb::create_migrated("media_recon").await;
    let store = db.store(5).await;
    let actor = owner_id(&store, "media-recon").await;
    let provider_id = Uuid::now_v7();
    let provider_etag = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers (id, name, kind, state, auth_mode, etag, created_by) \
         VALUES ($1, 'recon-provider', 'openai', 'active', 'api_key', $2, $3)",
    )
    .bind(provider_id)
    .bind(provider_etag)
    .bind(actor)
    .execute(store.pool())
    .await
    .unwrap();
    let provider_revision_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO provider_revisions \
         (id, provider_id, revision, name, kind, auth_mode, connector_ready, source_etag, \
          activated_by) \
         VALUES ($1, $2, 1, 'recon-provider', 'openai', 'api_key', true, $3, $4)",
    )
    .bind(provider_revision_id)
    .bind(provider_id)
    .bind(provider_etag)
    .bind(actor)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query("UPDATE providers SET active_revision_id = $1 WHERE id = $2")
        .bind(provider_revision_id)
        .bind(provider_id)
        .execute(store.pool())
        .await
        .unwrap();
    let generation_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO runtime_generations (id, compiled_release, release_sha256, created_by) \
         VALUES ($1, '{}'::text::bytea, $2, $3)",
    )
    .bind(generation_id)
    .bind([0_u8; 32].as_slice())
    .bind(actor)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO runtime_generation_provider_configs \
         (runtime_generation_id, provider_id, kind, auth_mode, provider_revision_id) \
         VALUES ($1, $2, 'openai', 'api_key', $3)",
    )
    .bind(generation_id)
    .bind(provider_id)
    .bind(provider_revision_id)
    .execute(store.pool())
    .await
    .unwrap();
    let api_key_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO api_keys (id, lookup_id, secret_digest, name, created_by) \
         VALUES ($1, 'olpv2recon01', $2, 'recon', $3)",
    )
    .bind(api_key_id)
    .bind([9_u8; 32].as_slice())
    .bind(actor)
    .execute(store.pool())
    .await
    .unwrap();

    let job_id = Uuid::now_v7();
    store
        .reserve_media_job(NewMediaJobReservation {
            id: job_id,
            runtime_generation_id: generation_id,
            api_key_id,
            provider_id,
            upstream_model: "video-model".to_owned(),
            route_slug: "video".to_owned(),
            operation: "video_create".parse().unwrap(),
            surface: "openai".parse().unwrap(),
        })
        .await
        .unwrap();
    let attached = store
        .attach_media_job_upstream(
            job_id,
            "upstream-1",
            MediaJobUpdate {
                state: MediaJobState::Queued,
                progress_percent: Some(0.0),
                content_available: false,
                expires_at: None,
                error_class: None,
                last_polled_at: Utc::now(),
            },
        )
        .await
        .unwrap();
    assert_eq!(attached.reconciliation_attempts, 0);

    // A client polls faster than the reconciler's 5s gate. The job is
    // deliberately unclaimable; it must not simultaneously read as stale.
    let polled_at = Utc::now();
    let refreshed = store
        .refresh_media_job(
            job_id,
            MediaJobUpdate {
                state: MediaJobState::Running,
                progress_percent: Some(20.0),
                content_available: false,
                expires_at: None,
                error_class: None,
                last_polled_at: polled_at,
            },
        )
        .await
        .unwrap();
    assert!(
        // PostgreSQL stores microseconds, so the round-tripped poll instant can
        // sit a few hundred nanoseconds below `polled_at`.
        refreshed.next_reconciliation_at > polled_at + Duration::seconds(4),
        "a client poll must carry the reconciliation deadline past the poll gate"
    );
    // The claim query refuses a job polled within the last 5 seconds. The
    // staleness predicate has to apply the same gate, or a job that is healthy
    // and unclaimable by design pins /ready at degraded.
    sqlx::query(
        "UPDATE async_media_jobs \
         SET next_reconciliation_at = now() - interval '10 minutes', last_polled_at = now() \
         WHERE id = $1",
    )
    .bind(job_id)
    .execute(store.pool())
    .await
    .unwrap();
    assert_eq!(
        store
            .media_reconciliation_summary(Utc::now())
            .await
            .unwrap()
            .stale,
        0,
        "a job a client is actively polling is healthy, not reconciliation-stale"
    );

    // A job nobody is polling any more is still reported stale.
    sqlx::query(
        "UPDATE async_media_jobs SET last_polled_at = now() - interval '1 minute' \
                 WHERE id = $1",
    )
    .bind(job_id)
    .execute(store.pool())
    .await
    .unwrap();
    assert_eq!(
        store
            .media_reconciliation_summary(Utc::now())
            .await
            .unwrap()
            .stale,
        1
    );

    // E5: the claim increments the attempt counter on every pass. A clean
    // finish must clear it so the next transient error backs off from zero.
    sqlx::query(
        "UPDATE async_media_jobs SET last_polled_at = NULL, \
                next_reconciliation_at = now() - interval '1 minute' WHERE id = $1",
    )
    .bind(job_id)
    .execute(store.pool())
    .await
    .unwrap();
    for _ in 0..3 {
        let claimed = store
            .claim_media_reconciliation_jobs(Utc::now(), 4)
            .await
            .unwrap();
        let job = claimed.iter().find(|job| job.id == job_id).unwrap();
        let claim_id = job.reconciliation_claim_id.unwrap();
        store
            .finish_media_reconciliation(job_id, claim_id, Utc::now() - Duration::minutes(1), None)
            .await
            .unwrap();
    }
    assert_eq!(
        store
            .media_job(job_id)
            .await
            .unwrap()
            .reconciliation_attempts,
        0,
        "a successful reconciliation must not leave a failure count behind"
    );

    let claimed = store
        .claim_media_reconciliation_jobs(Utc::now(), 4)
        .await
        .unwrap();
    let claim_id = claimed
        .iter()
        .find(|job| job.id == job_id)
        .unwrap()
        .reconciliation_claim_id
        .unwrap();
    store
        .finish_media_reconciliation(
            job_id,
            claim_id,
            Utc::now() + Duration::seconds(5),
            Some("upstream_unavailable"),
        )
        .await
        .unwrap();
    assert_eq!(
        store
            .media_job(job_id)
            .await
            .unwrap()
            .reconciliation_attempts,
        1,
        "a failing reconciliation keeps its consecutive-failure count"
    );
}

// E6: a rejection at one bucket used to roll back the increments the wider
// buckets had already taken, so the source and global ceilings never advanced
// and the expiry sweep never ran.
#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn a_rejected_public_auth_attempt_still_consumes_the_wider_buckets() {
    let db = olp_db::test_support::TestDb::create_migrated("auth_admit").await;
    let store = db.store(3).await;

    let source = [11_u8; 32];
    let source_target = [22_u8; 32];
    for _ in 0..5 {
        assert!(
            store
                .admit_local_login_attempt(source, source_target)
                .await
                .unwrap()
        );
    }
    for _ in 0..3 {
        assert!(
            !store
                .admit_local_login_attempt(source, source_target)
                .await
                .unwrap(),
            "the 5/min source_target bucket must stay saturated"
        );
    }

    let attempts: Vec<(String, i32)> = sqlx::query_as(
        "SELECT scope, attempts FROM public_auth_rate_limits \
         WHERE action = 'local_login' ORDER BY scope",
    )
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert_eq!(
        attempts,
        [
            ("global".to_owned(), 8),
            ("source".to_owned(), 8),
            // The saturated bucket itself stops counting; the wider ones the
            // attempt already passed through must not lose their increments.
            ("source_target".to_owned(), 5),
        ],
        "rejected attempts must still count against every bucket they reached"
    );

    // A rejection also has to run the expiry sweep, or a rejected-only stream
    // of traffic leaves stale rows behind forever.
    sqlx::query(
        "INSERT INTO public_auth_rate_limits \
         (action, scope, key_digest, window_started_at, attempts) \
         VALUES ('local_login', 'source', $1, now() - interval '20 minutes', 1)",
    )
    .bind([33_u8; 32].as_slice())
    .execute(store.pool())
    .await
    .unwrap();
    assert!(
        !store
            .admit_local_login_attempt(source, source_target)
            .await
            .unwrap()
    );
    let stale: i64 = sqlx::query_scalar(
        "SELECT count(*)::bigint FROM public_auth_rate_limits \
         WHERE window_started_at <= now() - interval '10 minutes'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(
        stale, 0,
        "the expiry sweep must run on the rejection path too"
    );
}

// E7: an invitation is an out-of-band grant. Demoting or deactivating the
// inviter revokes their sessions; the token they minted must not outlive that.
// E12: an invitation that merely timed out must not be rewritten as revoked
// and attributed to an unrelated operator.
#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn invitations_do_not_outlive_the_inviter_and_expiry_is_not_revocation() {
    let db = olp_db::test_support::TestDb::create_migrated("invitations").await;
    let store = db.store(5).await;
    let master_key = MasterKey::new(1, [17; 32]);
    let founder = owner_id(&store, "invitations").await;

    // A second owner does the inviting, so demoting them cannot trip the
    // last-owner guard.
    let Outcome::Executed {
        value: co_owner_invitation,
        ..
    } = store
        .create_invitation(
            NewInvitation {
                email: "co-owner@invitations.test".to_owned(),
                role: Role::Owner,
                expires_at: Utc::now() + Duration::days(7),
                actor: founder,
                idempotency_key: "invite-co-owner-01".to_owned(),
            },
            Replayable::new([1; 32], &master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap()
    else {
        panic!("the invitation must execute");
    };
    let co_owner = store
        .accept_invitation(
            AcceptInvitation {
                token: co_owner_invitation.material.token().to_owned(),
                display_name: "Co-owner".to_owned(),
                password_hash: hash("correct horse battery staple").unwrap(),
            },
            &SessionMaterial::generate(),
            Duration::hours(1),
        )
        .await
        .unwrap()
        .user;

    let Outcome::Executed { value: granted, .. } = store
        .create_invitation(
            NewInvitation {
                email: "grantee@invitations.test".to_owned(),
                role: Role::Owner,
                expires_at: Utc::now() + Duration::days(7),
                actor: co_owner.id,
                idempotency_key: "invite-grantee-01".to_owned(),
            },
            Replayable::new([2; 32], &master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap()
    else {
        panic!("the invitation must execute");
    };

    // Demote the inviter. Their sessions are revoked; so is this grant.
    store
        .update_user_access(
            co_owner.id,
            Some(Role::Viewer),
            None,
            co_owner.etag,
            founder,
        )
        .await
        .unwrap();
    let error = store
        .accept_invitation(
            AcceptInvitation {
                token: granted.material.token().to_owned(),
                display_name: "Grantee".to_owned(),
                password_hash: hash("correct horse battery staple").unwrap(),
            },
            &SessionMaterial::generate(),
            Duration::hours(1),
        )
        .await
        .unwrap_err();
    assert!(matches!(error, IdentityError::InvitationUnavailable));
    let revoked_by: Option<Uuid> =
        sqlx::query_scalar("SELECT revoked_by FROM invitations WHERE id = $1")
            .bind(granted.invitation.id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(
        revoked_by,
        Some(founder),
        "losing ManageAccess must retire the invitations the user minted"
    );

    // E12: a timed-out invitation is freed from the pending-email index
    // without stamping anyone's revocation intent.
    let Outcome::Executed {
        value: timed_out, ..
    } = store
        .create_invitation(
            NewInvitation {
                email: "lapsed@invitations.test".to_owned(),
                role: Role::Viewer,
                expires_at: Utc::now() + Duration::days(1),
                actor: founder,
                idempotency_key: "invite-lapsed-01".to_owned(),
            },
            Replayable::new([3; 32], &master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap()
    else {
        panic!("the invitation must execute");
    };
    // `expires_at > created_at` is enforced, so age the whole row.
    sqlx::query(
        "UPDATE invitations \
         SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour' \
         WHERE id = $1",
    )
    .bind(timed_out.invitation.id)
    .execute(store.pool())
    .await
    .unwrap();
    store
        .create_invitation(
            NewInvitation {
                email: "lapsed@invitations.test".to_owned(),
                role: Role::Viewer,
                expires_at: Utc::now() + Duration::days(1),
                actor: founder,
                idempotency_key: "invite-lapsed-02".to_owned(),
            },
            Replayable::new([4; 32], &master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap();
    let lapsed: (Option<DateTime<Utc>>, Option<DateTime<Utc>>, Option<Uuid>) =
        sqlx::query_as("SELECT expired_at, revoked_at, revoked_by FROM invitations WHERE id = $1")
            .bind(timed_out.invitation.id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert!(lapsed.0.is_some(), "the timeout must be recorded as expiry");
    assert!(
        lapsed.1.is_none() && lapsed.2.is_none(),
        "a timeout must not be rewritten as an operator's revocation"
    );
}

// E8: verification happens in Rust, so truncating to `limit` in SQL let a run
// of corrupt releases hide every intact one behind it.
#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn a_run_of_corrupt_releases_does_not_hide_an_intact_older_one() {
    let db = olp_db::test_support::TestDb::create_migrated("release_scan").await;
    let store = db.store(3).await;
    let actor = owner_id(&store, "release-scan").await;
    let valid = store.compile_and_publish_runtime(actor).await.unwrap();

    for _ in 0..4 {
        sqlx::query(
            "INSERT INTO runtime_generations (id, compiled_release, release_sha256, created_by) \
             VALUES ($1, 'corrupt'::text::bytea, $2, $3)",
        )
        .bind(Uuid::now_v7())
        .bind([0_u8; 32].as_slice())
        .bind(actor)
        .execute(store.pool())
        .await
        .unwrap();
    }

    let releases = store
        .recent_valid_runtime_releases_after(2, None)
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
    let db = olp_db::test_support::TestDb::create_migrated("worker_age").await;
    let store = db.store(3).await;
    store
        .report_worker_task_checkpoint(
            WorkerTask::Maintenance,
            WorkerTaskCheckpointOutcome::Success,
            true,
        )
        .await
        .unwrap();

    let fresh = store.worker_task_health().await.unwrap();
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
    .execute(store.pool())
    .await
    .unwrap();
    let aged = store.worker_task_health().await.unwrap();
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
    let db = olp_db::test_support::TestDb::create_migrated("outbox_progress").await;
    let store = db.store(4).await;

    let leader = store.acquire_runtime_outbox_leader().await.unwrap();
    leader.release().await.unwrap();
    sqlx::query("UPDATE runtime_outbox_health SET owner_active = true WHERE singleton")
        .execute(store.pool())
        .await
        .unwrap();

    let successor = store.acquire_runtime_outbox_leader().await.unwrap();
    let counters = store.worker_recovery_counters().await.unwrap();
    assert_eq!(counters.runtime_outbox_abandoned_ownership, 1);
    let status = store.runtime_outbox_status().await.unwrap();
    assert!(status.owner_active);
    assert!(
        status.last_progress_at.is_none(),
        "a takeover must not report progress the publisher never made"
    );
    let tasks = store.worker_task_health().await.unwrap();
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
