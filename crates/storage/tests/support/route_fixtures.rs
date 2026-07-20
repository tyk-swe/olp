use sqlx::PgPool;
use uuid::Uuid;

pub const LIFECYCLE_OPERATIONS: [&str; 4] =
    ["video_create", "video_get", "video_content", "video_delete"];

#[derive(Clone, Copy)]
pub struct ProviderFixture {
    pub provider_id: Uuid,
    pub model_id: Uuid,
}

pub struct DraftFixture {
    pub id: Uuid,
    pub etag: Uuid,
}

pub async fn insert_provider(pool: &PgPool, actor: Uuid, name: &str) -> ProviderFixture {
    let provider = ProviderFixture {
        provider_id: Uuid::now_v7(),
        model_id: Uuid::now_v7(),
    };
    let etag = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers \
         (id, name, kind, state, endpoint, auth_mode, etag, created_by) \
         VALUES ($1, $2, 'openai', 'active', 'https://api.example.test/v1/', \
                 'api_key', $3, $4)",
    )
    .bind(provider.provider_id)
    .bind(name)
    .bind(etag)
    .bind(actor)
    .execute(pool)
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO provider_models \
         (id, provider_id, upstream_model, display_name, enabled, discovered_at) \
         VALUES ($1, $2, $3, $3, true, now())",
    )
    .bind(provider.model_id)
    .bind(provider.provider_id)
    .bind(format!("{name}-model"))
    .execute(pool)
    .await
    .unwrap();
    let revision =
        insert_provider_revision(pool, actor, provider, 1, true, &LIFECYCLE_OPERATIONS).await;
    sqlx::query("UPDATE providers SET active_revision_id = $1 WHERE id = $2")
        .bind(revision)
        .bind(provider.provider_id)
        .execute(pool)
        .await
        .unwrap();
    provider
}

pub async fn insert_provider_revision(
    pool: &PgPool,
    actor: Uuid,
    provider: ProviderFixture,
    revision: i32,
    model_enabled: bool,
    operations: &[&str],
) -> Uuid {
    let revision_id = Uuid::now_v7();
    let revision_model_id = Uuid::now_v7();
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
    .bind(provider.provider_id)
    .execute(pool)
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO provider_revision_models \
         (id, provider_revision_id, source_provider_model_id, upstream_model, \
          display_name, enabled, discovered_at) \
         SELECT $1, $2, pm.id, pm.upstream_model, pm.display_name, $3, now() \
         FROM provider_models pm WHERE pm.id = $4",
    )
    .bind(revision_model_id)
    .bind(revision_id)
    .bind(model_enabled)
    .bind(provider.model_id)
    .execute(pool)
    .await
    .unwrap();
    for operation in operations {
        let mode = if *operation == "video_create" {
            "async"
        } else {
            "unary"
        };
        sqlx::query(
            "INSERT INTO provider_revision_capabilities \
             (provider_revision_model_id, operation, surface, mode, source, certified_at) \
             VALUES ($1, $2, 'openai', $3, 'certified', now())",
        )
        .bind(revision_model_id)
        .bind(operation)
        .bind(mode)
        .execute(pool)
        .await
        .unwrap();
    }
    revision_id
}
