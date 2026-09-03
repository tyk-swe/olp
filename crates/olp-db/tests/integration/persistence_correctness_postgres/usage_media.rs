use super::*;

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
    sqlx::query("SELECT set_config('olp.usage_rollup_writer', 'additive-v3', true)")
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
