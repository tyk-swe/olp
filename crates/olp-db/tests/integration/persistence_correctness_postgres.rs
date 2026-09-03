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

mod auth_identity;
mod route_history;
mod runtime_health;
mod usage_media;
