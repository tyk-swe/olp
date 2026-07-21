#[path = "support/route_fixtures.rs"]
mod route_fixtures;

use olp_storage::{
    ConfigurationError, InstallationSetupInput, PgStore, RuntimeCompileError, SessionMaterial,
};
use route_fixtures::{
    DraftFixture, LIFECYCLE_OPERATIONS, ProviderFixture, insert_provider, insert_provider_revision,
};
use sqlx::{PgPool, Row};
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn activation_revalidates_current_revisions_and_preserves_live_media_targets() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 5).await.unwrap();
    store.migrate().await.unwrap();
    let (owner, _) = store
        .setup_installation_with_session(
            InstallationSetupInput {
                installation_name: "Route revalidation".to_owned(),
                email: "owner@route-revalidation.test".to_owned(),
                display_name: "Owner".to_owned(),
                password_hash: "test-password-hash".to_owned(),
            },
            &SessionMaterial::generate(),
            chrono::Duration::hours(1),
        )
        .await
        .unwrap();
    let actor = owner.user_id;

    let first = insert_provider(store.pool(), actor, "video-primary").await;
    let second = insert_provider(store.pool(), actor, "video-fallback").await;
    let (route_id, active_revision_id) =
        insert_active_route(store.pool(), actor, first.model_id).await;

    // This replacement was valid before a video was created. Once the live
    // job exists, activation must not replace its exact pinned target with a
    // merely capability-equivalent fallback.
    let missing_target =
        insert_route_draft(store.pool(), actor, active_revision_id, &[second.model_id]).await;
    let (missing_target_etag, _) = store
        .validate_route_draft(missing_target.id, missing_target.etag, actor)
        .await
        .unwrap();
    let media_job_id = insert_media_job(store.pool(), actor, first, route_id).await;
    let error = store
        .activate_route_draft(
            missing_target.id,
            missing_target_etag,
            actor,
            "route-revalidate-missing-target",
        )
        .await
        .unwrap_err();
    assert_invalid_route_contains(&error, "requires its exact provider/model target");

    // A second draft keeps the exact target and validates while its current
    // provider revision still has the complete lifecycle tuple.
    let stale_capability = insert_route_draft(
        store.pool(),
        actor,
        active_revision_id,
        &[first.model_id, second.model_id],
    )
    .await;
    let (stale_capability_etag, _) = store
        .validate_route_draft(stale_capability.id, stale_capability.etag, actor)
        .await
        .unwrap();

    // Make a newer immutable revision authoritative after validation. The
    // fallback still covers every route operation, but the media job is pinned
    // to the primary model and therefore specifically requires video_content.
    let incomplete_revision = insert_provider_revision(
        store.pool(),
        actor,
        first,
        2,
        true,
        &["video_create", "video_get", "video_delete"],
    )
    .await;
    let mut preceding_publication = store.pool().begin().await.unwrap();
    sqlx::query("SELECT pg_advisory_xact_lock(87189184533076)")
        .execute(&mut *preceding_publication)
        .await
        .unwrap();
    sqlx::query("UPDATE providers SET active_revision_id = $1 WHERE id = $2")
        .bind(incomplete_revision)
        .bind(first.provider_id)
        .execute(&mut *preceding_publication)
        .await
        .unwrap();
    let activation_store = store.clone();
    let activation = tokio::spawn(async move {
        activation_store
            .activate_route_draft(
                stale_capability.id,
                stale_capability_etag,
                actor,
                "route-revalidate-stale-capability",
            )
            .await
    });
    let mut activation_is_waiting = false;
    for _ in 0..100 {
        activation_is_waiting = sqlx::query_scalar::<_, i64>(
            "SELECT count(*)::bigint FROM pg_locks \
             WHERE locktype = 'advisory' AND NOT granted",
        )
        .fetch_one(store.pool())
        .await
        .unwrap()
            > 0;
        if activation_is_waiting {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    assert!(
        activation_is_waiting,
        "route activation did not wait behind the preceding publication"
    );
    preceding_publication.commit().await.unwrap();

    let error = activation.await.unwrap().unwrap_err();
    assert_invalid_route_contains(&error, "exact certified lifecycle capability");
    let message = error.to_string();
    assert!(message.contains(&media_job_id.to_string()));
    assert!(message.contains("video_content"));

    // The compiler is a final defense for normalized state changed outside
    // the normal activation API: an active route cannot publish when none of
    // its targets can execute one of its declared operations.
    let error = store.compile_and_publish_runtime(actor).await.unwrap_err();
    assert!(matches!(
        error,
        RuntimeCompileError::InvalidConfiguration(ref message)
            if message.contains("no eligible target") && message.contains("VideoContent")
    ));

    // Disabled models are removed from compilation rather than leaking into a
    // runtime snapshot. With no remaining target, snapshot validation rejects
    // the release.
    let disabled_revision =
        insert_provider_revision(store.pool(), actor, first, 3, false, &[]).await;
    sqlx::query("UPDATE providers SET active_revision_id = $1 WHERE id = $2")
        .bind(disabled_revision)
        .bind(first.provider_id)
        .execute(store.pool())
        .await
        .unwrap();
    let error = store.compile_and_publish_runtime(actor).await.unwrap_err();
    assert!(matches!(
        error,
        RuntimeCompileError::InvalidConfiguration(ref message)
            if message.contains("at least one target")
    ));

    let generations: i64 = sqlx::query_scalar("SELECT count(*) FROM runtime_generations")
        .fetch_one(store.pool())
        .await
        .unwrap();
    assert_eq!(generations, 0, "invalid snapshots must never be published");
}

async fn insert_active_route(pool: &PgPool, actor: Uuid, model_id: Uuid) -> (Uuid, Uuid) {
    let draft_id = Uuid::now_v7();
    let route_id = Uuid::now_v7();
    let revision_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO route_drafts \
         (id, routing_id, slug, state, overall_timeout_ms, max_attempts, etag, created_by) \
         VALUES ($1, $2, 'video', 'validated', 30000, 1, $3, $4)",
    )
    .bind(draft_id)
    .bind(Uuid::now_v7())
    .bind(Uuid::now_v7())
    .bind(actor)
    .execute(pool)
    .await
    .unwrap();
    insert_route_operations(pool, draft_id, "route_draft_operations", "route_draft_id").await;
    sqlx::query(
        "INSERT INTO route_draft_targets \
         (id, routing_id, route_draft_id, provider_model_id, priority, weight, timeout_ms, position) \
         VALUES ($1, $2, $3, $4, 0, 1, 20000, 0)",
    )
    .bind(Uuid::now_v7())
    .bind(Uuid::now_v7())
    .bind(draft_id)
    .bind(model_id)
    .execute(pool)
    .await
    .unwrap();
    sqlx::query("INSERT INTO routes (id, slug, created_by) VALUES ($1, 'video', $2)")
        .bind(route_id)
        .bind(actor)
        .execute(pool)
        .await
        .unwrap();
    sqlx::query(
        "INSERT INTO route_revisions \
         (id, route_id, routing_id, revision, slug, overall_timeout_ms, max_attempts, source_draft_id, activated_by) \
         VALUES ($1, $2, $3, 1, 'video', 30000, 1, $4, $5)",
    )
    .bind(revision_id)
    .bind(route_id)
    .bind(route_id)
    .bind(draft_id)
    .bind(actor)
    .execute(pool)
    .await
    .unwrap();
    insert_route_operations(
        pool,
        revision_id,
        "route_revision_operations",
        "route_revision_id",
    )
    .await;
    sqlx::query(
        "INSERT INTO route_revision_targets \
         (id, routing_id, route_revision_id, provider_model_id, priority, weight, timeout_ms, position) \
         VALUES ($1, $2, $3, $4, 0, 1, 20000, 0)",
    )
    .bind(Uuid::now_v7())
    .bind(Uuid::now_v7())
    .bind(revision_id)
    .bind(model_id)
    .execute(pool)
    .await
    .unwrap();
    (route_id, revision_id)
}

async fn insert_route_draft(
    pool: &PgPool,
    actor: Uuid,
    based_on_revision: Uuid,
    model_ids: &[Uuid],
) -> DraftFixture {
    let fixture = DraftFixture {
        id: Uuid::now_v7(),
        etag: Uuid::now_v7(),
    };
    sqlx::query(
        "INSERT INTO route_drafts \
         (id, routing_id, slug, state, overall_timeout_ms, max_attempts, etag, based_on_revision_id, created_by) \
         VALUES ($1, $2, 'video', 'draft', 30000, 1, $3, $4, $5)",
    )
    .bind(fixture.id)
    .bind(Uuid::now_v7())
    .bind(fixture.etag)
    .bind(based_on_revision)
    .bind(actor)
    .execute(pool)
    .await
    .unwrap();
    insert_route_operations(pool, fixture.id, "route_draft_operations", "route_draft_id").await;
    for (position, model_id) in model_ids.iter().enumerate() {
        sqlx::query(
            "INSERT INTO route_draft_targets \
             (id, routing_id, route_draft_id, provider_model_id, priority, weight, timeout_ms, position) \
             VALUES ($1, $2, $3, $4, 0, 1, 20000, $5)",
        )
        .bind(Uuid::now_v7())
        .bind(Uuid::now_v7())
        .bind(fixture.id)
        .bind(model_id)
        .bind(i32::try_from(position).unwrap())
        .execute(pool)
        .await
        .unwrap();
    }
    fixture
}

async fn insert_route_operations(pool: &PgPool, id: Uuid, table: &str, id_column: &str) {
    let query = match (table, id_column) {
        ("route_draft_operations", "route_draft_id") => {
            "INSERT INTO route_draft_operations (route_draft_id, operation) VALUES ($1, $2)"
        }
        ("route_revision_operations", "route_revision_id") => {
            "INSERT INTO route_revision_operations (route_revision_id, operation) VALUES ($1, $2)"
        }
        _ => unreachable!("route fixture table and ID column are fixed"),
    };
    for operation in LIFECYCLE_OPERATIONS {
        sqlx::query(query)
            .bind(id)
            .bind(operation)
            .execute(pool)
            .await
            .unwrap();
    }
}

async fn insert_media_job(
    pool: &PgPool,
    actor: Uuid,
    provider: ProviderFixture,
    route_id: Uuid,
) -> Uuid {
    let api_key_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO api_keys (id, lookup_id, secret_digest, name, created_by) \
         VALUES ($1, $2, $3, 'media key', $4)",
    )
    .bind(api_key_id)
    .bind(format!("media_{}", &api_key_id.simple().to_string()[..12]))
    .bind(vec![0_u8; 32])
    .bind(actor)
    .execute(pool)
    .await
    .unwrap();
    let upstream_model: String =
        sqlx::query("SELECT upstream_model FROM provider_models WHERE id = $1")
            .bind(provider.model_id)
            .fetch_one(pool)
            .await
            .unwrap()
            .get("upstream_model");
    let job_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO async_media_jobs \
         (id, upstream_job_id, api_key_id, provider_id, provider_model, route_slug, \
          operation, surface, state, lifecycle_state) \
         SELECT $1, 'upstream-video-1', $2, $3, $4, r.slug, 'video_create', \
                'openai', 'queued', 'active' FROM routes r WHERE r.id = $5",
    )
    .bind(job_id)
    .bind(api_key_id)
    .bind(provider.provider_id)
    .bind(upstream_model)
    .bind(route_id)
    .execute(pool)
    .await
    .unwrap();
    job_id
}

fn assert_invalid_route_contains(error: &ConfigurationError, expected: &str) {
    assert!(matches!(
        error,
        ConfigurationError::InvalidRoute(message) if message.contains(expected)
    ));
}
