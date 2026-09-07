use chrono::Duration;
use chrono::Timelike;
use chrono::Utc;
use olp::access::audit::Filters as AuditFilters;
use olp::access::identity::InstallationSetupInput;
use olp::crypto::envelope::MasterKey;
use olp::crypto::password::hash;
use olp::database::RequestProvenance;
use olp::database::cursor::Error;
use olp::database::idempotency::Outcome as IdempotencyOutcome;
use olp::database::idempotency::Replayable;
use olp::database::idempotency::Response;
use olp::protocols::canonical::identity::Surface;
use olp::settings::repository::LimitsValkeyUnavailablePolicy;
use olp::usage::emitter::Event;
use olp::usage::emitter::RequestAttemptMetadata;
use olp::usage::emitter::RequestAttemptUsageMetadata;
use olp::usage::emitter::Snapshot;
use olp::usage::history::RequestFilters;
use olp::usage::ingestion::delivery_health::ConsumerState;
use olp::usage::ingestion::persistence::Outcome as IngestionOutcome;
use olp::usage::ingestion::reconciliation::Gap;
use olp::usage::ingestion::reconciliation::GatewayEpochState;
use olp::usage::pricing::PriceInput;
use olp::usage::reports::Dimension;
use olp::usage::reports::Filters;
use olp::usage::reports::Granularity;
use rust_decimal::Decimal;
use sqlx::PgPool;
use uuid::Uuid;

mod attempt_accounting;
mod query_contracts;
mod retention;

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn operations_queries_pricing_rollups_health_and_completeness_reconcile() {
    let db = olp::test_support::TestDb::create_migrated("operations").await;
    let pool = db.pool(5).await;
    let owner = olp::access::identity::setup::setup_installation(
        &pool,
        &olp::database::RequestProvenance::default(),
        InstallationSetupInput {
            installation_name: "Operations integration".to_owned(),
            email: "owner@example.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash("correct horse battery staple").unwrap(),
        },
    )
    .await
    .unwrap();
    let provider_id = Uuid::now_v7();
    let master_key = MasterKey::new(1, [29; 32]);
    let api_key_id = Uuid::now_v7();
    let generation_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers
         (id, name, kind, state, auth_mode, etag, created_by,
          last_probe_at, last_probe_status, last_probe_detail)
         VALUES ($1, 'operations-provider', 'openai', 'active', 'api_key', $2, $3,
                 now(), 'succeeded', 'mock probe succeeded')",
    )
    .bind(provider_id)
    .bind(Uuid::now_v7())
    .bind(owner.user_id)
    .execute(&pool)
    .await
    .unwrap();

    sqlx::query(
        "INSERT INTO api_keys
         (id, lookup_id, secret_digest, name, created_by)
         VALUES ($1, 'olpv3oper001', $2, 'operations test', $3)",
    )
    .bind(api_key_id)
    .bind([9_u8; 32].as_slice())
    .bind(owner.user_id)
    .execute(&pool)
    .await
    .unwrap();

    sqlx::query(
        "INSERT INTO runtime_generations
         (id, compiled_release, release_sha256, created_by)
         VALUES ($1, $2, $3, $4)",
    )
    .bind(generation_id)
    .bind([1_u8].as_slice())
    .bind([2_u8; 32].as_slice())
    .bind(owner.user_id)
    .execute(&pool)
    .await
    .unwrap();

    let fixture = query_contracts::exercise(
        &pool,
        owner.user_id,
        provider_id,
        &master_key,
        api_key_id,
        generation_id,
    )
    .await;
    attempt_accounting::exercise(&pool, owner.user_id, provider_id, api_key_id, generation_id)
        .await;
    retention::exercise(&pool, provider_id, api_key_id, generation_id, fixture).await;
}
