use chrono::{Duration, Utc};
use olp_storage::{
    CatalogError, MediaJobError, MediaJobFilters, MediaJobLifecycle, MediaJobOrder, MediaJobState,
    MediaJobUpdate, NewMediaJobReservation, NewOwner, PgStore, hash_password,
};
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn media_job_lifecycle_is_paginated_metadata_only_and_transition_checked() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 5).await.unwrap();
    store.migrate().await.unwrap();
    let owner = store
        .setup_owner(NewOwner {
            organization_name: "Media jobs integration".to_owned(),
            email: "owner@example.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash_password("correct horse battery staple").unwrap(),
        })
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
    .execute(store.pool())
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
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query("UPDATE providers SET active_revision_id = $1 WHERE id = $2")
        .bind(provider_revision_id)
        .bind(provider_id)
        .execute(store.pool())
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
    .execute(store.pool())
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
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO api_keys
         (id, lookup_id, secret_digest, name, created_by)
         VALUES ($1, 'olpv2media01', $2, 'media test', $3)",
    )
    .bind(api_key_id)
    .bind([7_u8; 32].as_slice())
    .bind(owner.user_id)
    .execute(store.pool())
    .await
    .unwrap();

    let first_id = Uuid::now_v7();
    let reservation = store
        .reserve_media_job(NewMediaJobReservation {
            id: first_id,
            runtime_generation_id,
            api_key_id,
            provider_id,
            provider_model: "video-model".to_owned(),
            route_slug: "video-default".to_owned(),
            operation: "video_create".parse().unwrap(),
            surface: "open_ai".parse().unwrap(),
        })
        .await
        .unwrap();
    assert_eq!(reservation.lifecycle, MediaJobLifecycle::Creating);
    assert_eq!(
        reservation.runtime_generation_id,
        Some(runtime_generation_id)
    );
    assert_eq!(reservation.provider_revision_id, Some(provider_revision_id));
    let first = store
        .attach_media_job_upstream(
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
    store
        .reserve_media_job(NewMediaJobReservation {
            id: second_id,
            runtime_generation_id,
            api_key_id,
            provider_id,
            provider_model: "video-model".to_owned(),
            route_slug: "video-default".to_owned(),
            operation: "video_create".parse().unwrap(),
            surface: "open_ai".parse().unwrap(),
        })
        .await
        .unwrap();
    let second = store
        .attach_media_job_upstream(
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

    let page = store
        .media_jobs(&MediaJobFilters::default(), None, 1)
        .await
        .unwrap();
    assert_eq!(page.items.len(), 1);
    assert_eq!(page.items[0].id, second.id);
    let cursor = olp_storage::TimestampCursor::parse(page.next_cursor.as_deref().unwrap()).unwrap();
    let next = store
        .media_jobs(&MediaJobFilters::default(), Some(&cursor), 1)
        .await
        .unwrap();
    assert_eq!(next.items[0].id, first.id);

    let client_filters = MediaJobFilters {
        api_key_id: Some(api_key_id),
        route_slugs: vec!["video-default".to_owned()],
        ..MediaJobFilters::default()
    };
    let oldest = store
        .media_jobs_after_id(&client_filters, None, MediaJobOrder::Ascending, 1)
        .await
        .unwrap();
    assert_eq!(oldest.items[0].id, first.id);
    assert_eq!(oldest.next_cursor, Some(first.id.to_string()));
    let newer = store
        .media_jobs_after_id(&client_filters, Some(first.id), MediaJobOrder::Ascending, 1)
        .await
        .unwrap();
    assert_eq!(newer.items[0].id, second.id);
    assert!(matches!(
        store
            .media_jobs_after_id(
                &client_filters,
                Some(Uuid::now_v7()),
                MediaJobOrder::Descending,
                1,
            )
            .await,
        Err(MediaJobError::Invalid(_))
    ));

    let poll_base = Utc::now();
    let running_refresh = store
        .refresh_media_job(
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
    let stale = store
        .refresh_media_job(
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

    let terminal = store
        .refresh_media_job(
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
    let regressed = store
        .refresh_media_job(
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
    let deleting = store.begin_media_job_deletion(second.id).await.unwrap();
    assert_eq!(deleting.lifecycle, MediaJobLifecycle::DeletePending);
    assert!(store.finalize_media_job_deletion(second.id).await.unwrap());
    assert!(!store.finalize_media_job_deletion(second.id).await.unwrap());
    assert_eq!(
        store.media_job(second.id).await.unwrap().lifecycle,
        MediaJobLifecycle::Deleted
    );

    let cleanup_id = Uuid::now_v7();
    store
        .reserve_media_job(NewMediaJobReservation {
            id: cleanup_id,
            runtime_generation_id,
            api_key_id,
            provider_id,
            provider_model: "video-model".to_owned(),
            route_slug: "video-default".to_owned(),
            operation: "video_create".parse().unwrap(),
            surface: "open_ai".parse().unwrap(),
        })
        .await
        .unwrap();
    store
        .mark_media_job_create_cleanup_pending(
            cleanup_id,
            "upstream-video-cleanup",
            "injected_attach_failure",
        )
        .await
        .unwrap();
    let pending = store
        .pending_media_reconciliation_jobs(api_key_id, 8)
        .await
        .unwrap();
    assert!(pending.iter().any(|record| record.id == cleanup_id));
    let claim_at = Utc::now();
    let (left, right) = tokio::join!(
        store.claim_media_reconciliation_jobs(claim_at, 8),
        store.claim_media_reconciliation_jobs(claim_at, 8),
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
    .execute(store.pool())
    .await
    .unwrap();
    let reclaimed = store
        .claim_media_reconciliation_jobs(claim_at, 8)
        .await
        .unwrap()
        .into_iter()
        .find(|record| record.id == cleanup_id)
        .unwrap();
    let second_claim_id = reclaimed.reconciliation_claim_id.unwrap();
    assert_ne!(first_claim_id, second_claim_id);
    store
        .finish_media_reconciliation(
            cleanup_id,
            second_claim_id,
            claim_at + Duration::seconds(5),
            Some("injected_retry"),
        )
        .await
        .unwrap();
    let checkpointed = store.media_job(cleanup_id).await.unwrap();
    assert_eq!(
        checkpointed.reconciliation_error.as_deref(),
        Some("injected_retry")
    );
    assert!(checkpointed.reconciliation_attempts >= 2);

    // Migration 0018 deliberately leaves pre-upgrade jobs unbound when no
    // historical runtime authority can be proven.
    let legacy_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO async_media_jobs (
            id, upstream_job_id, api_key_id, provider_id, provider_model,
            route_slug, operation, surface, state, lifecycle_state, progress_percent
         ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'queued', 'active', 0)",
    )
    .bind(legacy_id)
    .bind("upstream-video-pre-authority")
    .bind(api_key_id)
    .bind(provider_id)
    .bind("video-model")
    .bind("video-default")
    .bind("video_create")
    .bind("openai")
    .execute(store.pool())
    .await
    .unwrap();
    let legacy = store.media_job(legacy_id).await.unwrap();
    assert!(legacy.runtime_generation_id.is_none());
    assert!(legacy.provider_revision_id.is_none());

    let summary = store
        .media_reconciliation_summary(Utc::now() + Duration::minutes(10))
        .await
        .unwrap();
    assert!(summary.pending >= 1);
    assert!(summary.stale >= 1);
    assert!(summary.failed >= 1);
    assert_eq!(summary.unbound, 1);
    assert!(matches!(
        store
            .disable_provider_catalog(
                provider_id,
                provider_etag,
                owner.user_id,
                "media-provider-disable-01",
            )
            .await,
        Err(CatalogError::InUse)
    ));

    let columns: Vec<String> = sqlx::query_scalar(
        "SELECT column_name FROM information_schema.columns
         WHERE table_schema = 'public' AND table_name = 'async_media_jobs'",
    )
    .fetch_all(store.pool())
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
