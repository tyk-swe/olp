use chrono::Duration;
use chrono::Utc;
use olp::access::identity::InstallationSetupInput;
use olp::crypto::password::hash;
use olp::media::jobs::MediaJobError;
use olp::media::jobs::MediaJobFilters;
use olp::media::jobs::MediaJobLifecycle;
use olp::media::jobs::MediaJobOrder;
use olp::media::jobs::MediaJobState;
use olp::media::jobs::MediaJobUpdate;
use olp::media::jobs::NewMediaJobReservation;
use olp::providers::error::Error;
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn media_job_lifecycle_is_paginated_metadata_only_and_transition_checked() {
    let db = olp::test_support::TestDb::create_migrated("media_jobs").await;
    let pool = db.pool(5).await;
    let owner = olp::access::identity::setup::setup_installation(
        &pool,
        &olp::database::RequestProvenance::default(),
        InstallationSetupInput {
            installation_name: "Media jobs integration".to_owned(),
            email: "owner@example.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash("correct horse battery staple").unwrap(),
        },
    )
    .await
    .unwrap();
    let provider_id = Uuid::now_v7();
    let provider_etag = Uuid::now_v7();
    let api_key_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers
         (id, name, kind, state, auth_mode, etag, created_by)
         VALUES ($1, 'media-provider', 'openai', 'active', 'api_key', $2, $3)",
    )
    .bind(provider_id)
    .bind(provider_etag)
    .bind(owner.user_id)
    .execute(&pool)
    .await
    .unwrap();
    let provider_revision_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO provider_revisions
         (id, provider_id, revision, name, kind, auth_mode, connector_ready,
          source_etag, activated_by)
         VALUES ($1, $2, 1, 'media-provider', 'openai', 'api_key', true, $3, $4)",
    )
    .bind(provider_revision_id)
    .bind(provider_id)
    .bind(provider_etag)
    .bind(owner.user_id)
    .execute(&pool)
    .await
    .unwrap();
    sqlx::query("UPDATE providers SET active_revision_id = $1 WHERE id = $2")
        .bind(provider_revision_id)
        .bind(provider_id)
        .execute(&pool)
        .await
        .unwrap();
    sqlx::query(
        "WITH model AS (
             INSERT INTO provider_models
             (id, provider_id, upstream_model, display_name, enabled, discovered_at)
             VALUES (uuidv7(), $1, 'video-model', 'Video model', true, now()) RETURNING id
         ), revision_model AS (
             INSERT INTO provider_revision_models
             (id, provider_revision_id, source_provider_model_id, upstream_model,
              display_name, enabled, discovered_at)
             SELECT uuidv7(), $2, id, 'video-model', 'Video model', true, now() FROM model
             RETURNING id
         )
         INSERT INTO provider_revision_capabilities
         (provider_revision_model_id, operation, surface, mode, source, certified_at)
         SELECT id, operation, 'openai', 'unary', 'certified', now()
         FROM revision_model CROSS JOIN
              unnest(ARRAY['video_get', 'video_content', 'video_delete']) AS operation",
    )
    .bind(provider_id)
    .bind(provider_revision_id)
    .execute(&pool)
    .await
    .unwrap();
    let runtime_generation_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO runtime_generations
         (id, compiled_release, release_sha256, created_by)
         VALUES ($1, '{}'::text::bytea, $2, $3)",
    )
    .bind(runtime_generation_id)
    .bind([0_u8; 32].as_slice())
    .bind(owner.user_id)
    .execute(&pool)
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO runtime_generation_provider_configs
         (runtime_generation_id, provider_id, kind, auth_mode, provider_revision_id)
         VALUES ($1, $2, 'openai', 'api_key', $3)",
    )
    .bind(runtime_generation_id)
    .bind(provider_id)
    .bind(provider_revision_id)
    .execute(&pool)
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO api_keys
         (id, lookup_id, secret_digest, name, created_by)
         VALUES ($1, 'olpv3media01', $2, 'media test', $3)",
    )
    .bind(api_key_id)
    .bind([7_u8; 32].as_slice())
    .bind(owner.user_id)
    .execute(&pool)
    .await
    .unwrap();

    let first_id = Uuid::now_v7();
    let reservation = olp::media::jobs::lifecycle::reserve_media_job(
        &pool,
        NewMediaJobReservation {
            credential_version_id: None,
            id: first_id,
            runtime_generation_id,
            api_key_id,
            provider_id,
            upstream_model: "video-model".to_owned(),
            route_slug: "video-default".to_owned(),
            operation: "video_create".parse().unwrap(),
            surface: "openai".parse().unwrap(),
        },
    )
    .await
    .unwrap();
    assert_eq!(reservation.lifecycle, MediaJobLifecycle::Creating);
    assert_eq!(reservation.runtime_generation_id, runtime_generation_id);
    assert_eq!(reservation.provider_revision_id, provider_revision_id);
    let first = olp::media::jobs::lifecycle::attach_media_job_upstream(
        &pool,
        first_id,
        "upstream-video-1",
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
    assert_eq!(first.lifecycle, MediaJobLifecycle::Active);
    let second_id = Uuid::now_v7();
    olp::media::jobs::lifecycle::reserve_media_job(
        &pool,
        NewMediaJobReservation {
            credential_version_id: None,
            id: second_id,
            runtime_generation_id,
            api_key_id,
            provider_id,
            upstream_model: "video-model".to_owned(),
            route_slug: "video-default".to_owned(),
            operation: "video_create".parse().unwrap(),
            surface: "openai".parse().unwrap(),
        },
    )
    .await
    .unwrap();
    let second = olp::media::jobs::lifecycle::attach_media_job_upstream(
        &pool,
        second_id,
        "upstream-video-2",
        MediaJobUpdate {
            state: MediaJobState::Running,
            progress_percent: Some(10.0),
            content_available: false,
            expires_at: None,
            error_class: None,
            last_polled_at: Utc::now(),
        },
    )
    .await
    .unwrap();

    let page = olp::media::jobs::queries::media_jobs(&pool, &MediaJobFilters::default(), None, 1)
        .await
        .unwrap();
    assert_eq!(page.items.len(), 1);
    assert_eq!(page.items[0].id, second.id);
    let cursor =
        olp::database::cursor::Timestamp::parse(page.next_cursor.as_deref().unwrap()).unwrap();
    let next =
        olp::media::jobs::queries::media_jobs(&pool, &MediaJobFilters::default(), Some(&cursor), 1)
            .await
            .unwrap();
    assert_eq!(next.items[0].id, first.id);

    let client_filters = MediaJobFilters {
        api_key_id: Some(api_key_id),
        route_slugs: vec!["video-default".to_owned()],
        ..MediaJobFilters::default()
    };
    let oldest = olp::media::jobs::queries::media_jobs_after_id(
        &pool,
        &client_filters,
        None,
        MediaJobOrder::Ascending,
        1,
    )
    .await
    .unwrap();
    assert_eq!(oldest.items[0].id, first.id);
    assert_eq!(oldest.next_cursor, Some(first.id.to_string()));
    let newer = olp::media::jobs::queries::media_jobs_after_id(
        &pool,
        &client_filters,
        Some(first.id),
        MediaJobOrder::Ascending,
        1,
    )
    .await
    .unwrap();
    assert_eq!(newer.items[0].id, second.id);

    olp::media::jobs::lifecycle::begin_media_job_deletion(&pool, first.id)
        .await
        .unwrap();
    assert!(
        olp::media::jobs::lifecycle::finalize_media_job_deletion(&(pool), first.id)
            .await
            .unwrap()
    );
    let newer_after_cursor_deletion = olp::media::jobs::queries::media_jobs_after_id(
        &pool,
        &client_filters,
        Some(first.id),
        MediaJobOrder::Ascending,
        1,
    )
    .await
    .unwrap();
    assert_eq!(newer_after_cursor_deletion.items[0].id, second.id);

    assert!(matches!(
        olp::media::jobs::queries::media_jobs_after_id(
            &(pool),
            &client_filters,
            Some(Uuid::now_v7()),
            MediaJobOrder::Descending,
            1,
        )
        .await,
        Err(MediaJobError::Invalid(_))
    ));

    let poll_base = Utc::now();
    let running_refresh = olp::media::jobs::lifecycle::refresh_media_job(
        &pool,
        second.id,
        MediaJobUpdate {
            state: MediaJobState::Running,
            progress_percent: Some(60.0),
            content_available: false,
            expires_at: None,
            error_class: None,
            last_polled_at: poll_base + Duration::seconds(2),
        },
    )
    .await
    .unwrap();
    assert_eq!(running_refresh.progress_percent, Some(60.0));
    // Retrying a successful upstream attachment must not overwrite a newer
    // poll result while reporting the same durable upstream identity.
    let retry = olp::media::jobs::lifecycle::attach_media_job_upstream(
        &pool,
        second.id,
        "upstream-video-2",
        MediaJobUpdate {
            state: MediaJobState::Queued,
            progress_percent: Some(0.0),
            content_available: false,
            expires_at: None,
            error_class: None,
            last_polled_at: poll_base,
        },
    )
    .await
    .unwrap();
    assert_eq!(retry.state, MediaJobState::Running);
    assert_eq!(retry.progress_percent, Some(60.0));
    let stale = olp::media::jobs::lifecycle::refresh_media_job(
        &pool,
        second.id,
        MediaJobUpdate {
            state: MediaJobState::Queued,
            progress_percent: Some(5.0),
            content_available: false,
            expires_at: None,
            error_class: None,
            last_polled_at: poll_base + Duration::seconds(1),
        },
    )
    .await
    .unwrap();
    assert_eq!(stale.state, MediaJobState::Running);
    assert_eq!(stale.progress_percent, Some(60.0));

    let terminal = olp::media::jobs::lifecycle::refresh_media_job(
        &pool,
        second.id,
        MediaJobUpdate {
            state: MediaJobState::Succeeded,
            progress_percent: Some(100.0),
            content_available: true,
            expires_at: None,
            error_class: None,
            last_polled_at: poll_base + Duration::seconds(3),
        },
    )
    .await
    .unwrap();
    let regressed = olp::media::jobs::lifecycle::refresh_media_job(
        &pool,
        second.id,
        MediaJobUpdate {
            state: MediaJobState::Running,
            progress_percent: Some(100.0),
            content_available: false,
            expires_at: None,
            error_class: None,
            last_polled_at: poll_base + Duration::seconds(4),
        },
    )
    .await
    .unwrap();
    assert_eq!(regressed.state, MediaJobState::Succeeded);
    assert_eq!(regressed.etag, terminal.etag);
    assert!(regressed.content_available);

    // Polling changed the ETag after `second` was initially loaded. Durable
    // delete intent and finalization remain independent of that stale token.
    let deleting = olp::media::jobs::lifecycle::begin_media_job_deletion(&pool, second.id)
        .await
        .unwrap();
    assert_eq!(deleting.lifecycle, MediaJobLifecycle::DeletePending);
    assert!(
        olp::media::jobs::lifecycle::finalize_media_job_deletion(&(pool), second.id)
            .await
            .unwrap()
    );
    assert!(
        !olp::media::jobs::lifecycle::finalize_media_job_deletion(&(pool), second.id)
            .await
            .unwrap()
    );
    assert_eq!(
        olp::media::jobs::queries::media_job(&(pool), second.id)
            .await
            .unwrap()
            .lifecycle,
        MediaJobLifecycle::Deleted
    );

    let cleanup_id = Uuid::now_v7();
    olp::media::jobs::lifecycle::reserve_media_job(
        &pool,
        NewMediaJobReservation {
            credential_version_id: None,
            id: cleanup_id,
            runtime_generation_id,
            api_key_id,
            provider_id,
            upstream_model: "video-model".to_owned(),
            route_slug: "video-default".to_owned(),
            operation: "video_create".parse().unwrap(),
            surface: "openai".parse().unwrap(),
        },
    )
    .await
    .unwrap();
    olp::media::jobs::lifecycle::mark_media_job_create_cleanup_pending(
        &pool,
        cleanup_id,
        "upstream-video-cleanup",
        "injected_attach_failure",
    )
    .await
    .unwrap();
    let pending =
        olp::media::jobs::reconciliation::pending_media_reconciliation_jobs(&pool, api_key_id, 8)
            .await
            .unwrap();
    assert!(pending.iter().any(|record| record.id == cleanup_id));
    let claim_at = Utc::now();
    let (left, right) = tokio::join!(
        olp::media::jobs::reconciliation::claim_media_reconciliation_jobs(&(pool), claim_at, 8),
        olp::media::jobs::reconciliation::claim_media_reconciliation_jobs(&(pool), claim_at, 8),
    );
    let mut claimed = left.unwrap();
    claimed.extend(right.unwrap());
    let cleanup_claims = claimed
        .iter()
        .filter(|record| record.id == cleanup_id)
        .collect::<Vec<_>>();
    assert_eq!(cleanup_claims.len(), 1);
    let first_claim_id = cleanup_claims[0].reconciliation_claim_id.unwrap();

    // A crashed gateway's lease is recoverable by another replica after the
    // bounded deadline, with a distinct fencing token.
    sqlx::query(
        "UPDATE async_media_jobs SET reconciliation_claimed_until = $2,
                next_reconciliation_at = $2 WHERE id = $1",
    )
    .bind(cleanup_id)
    .bind(claim_at - Duration::seconds(1))
    .execute(&pool)
    .await
    .unwrap();
    let reclaimed =
        olp::media::jobs::reconciliation::claim_media_reconciliation_jobs(&pool, claim_at, 8)
            .await
            .unwrap()
            .into_iter()
            .find(|record| record.id == cleanup_id)
            .unwrap();
    let second_claim_id = reclaimed.reconciliation_claim_id.unwrap();
    assert_ne!(first_claim_id, second_claim_id);
    olp::media::jobs::reconciliation::finish_media_reconciliation(
        &pool,
        cleanup_id,
        second_claim_id,
        claim_at + Duration::seconds(5),
        Some("injected_retry"),
    )
    .await
    .unwrap();
    let checkpointed = olp::media::jobs::queries::media_job(&pool, cleanup_id)
        .await
        .unwrap();
    assert_eq!(
        checkpointed.reconciliation_error.as_deref(),
        Some("injected_retry")
    );
    assert!(checkpointed.reconciliation_attempts >= 2);

    let unbound_id = Uuid::now_v7();
    let unbound = sqlx::query(
        "INSERT INTO async_media_jobs (
            id, upstream_job_id, api_key_id, provider_id, provider_model,
            route_slug, operation, surface, state, lifecycle_state, progress_percent
         ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'queued', 'active', 0)",
    )
    .bind(unbound_id)
    .bind("upstream-video-pre-authority")
    .bind(api_key_id)
    .bind(provider_id)
    .bind("video-model")
    .bind("video-default")
    .bind("video_create")
    .bind("openai")
    .execute(&pool)
    .await
    .unwrap_err();
    assert_eq!(
        unbound
            .as_database_error()
            .and_then(sqlx::error::DatabaseError::code)
            .as_deref(),
        Some("23502")
    );

    let summary = olp::media::jobs::reconciliation::media_reconciliation_summary(
        &pool,
        Utc::now() + Duration::minutes(10),
    )
    .await
    .unwrap();
    assert!(summary.pending >= 1);
    assert!(summary.stale >= 1);
    assert!(summary.failed >= 1);
    assert!(matches!(
        olp::providers::repository::disable_provider(
            &(pool),
            &olp::database::RequestProvenance::default(),
            provider_id,
            provider_etag,
            owner.user_id,
            "media-provider-disable-01",
        )
        .await,
        Err(Error::InUse)
    ));

    let columns: Vec<String> = sqlx::query_scalar(
        "SELECT column_name FROM information_schema.columns
         WHERE table_schema = 'olp_v3' AND table_name = 'async_media_jobs'",
    )
    .fetch_all(&pool)
    .await
    .unwrap();
    for prohibited in [
        "prompt",
        "output",
        "content",
        "raw_headers",
        "credential",
        "file",
    ] {
        assert!(!columns.iter().any(|column| column == prohibited));
    }
}

mod reservation;
