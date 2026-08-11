use chrono::{Duration, Timelike, Utc};
use olp_db::{
    PgStore, idempotency::IdempotencyOutcome, idempotency::IdempotencyResponse,
    idempotency::ReplayableIdempotency, identity::InstallationSetupInput,
    operations::OperationsError, operations::PriceInput, operations::RequestFilters,
    request_metadata::RequestMetadataConsumerState, request_metadata::RequestMetadataGap,
    request_metadata::RequestMetadataGatewayEpochState,
    request_metadata::RequestMetadataPersistenceOutcome, security::MasterKey,
    security::hash_password, usage::UsageDimension, usage::UsageFilters, usage::UsageGranularity,
};
use olp_engine::{
    domain::Surface,
    inference::request_metadata::{
        RequestAttemptMetadata, RequestAttemptUsageMetadata, RequestMetadataBufferSnapshot,
        RequestMetadataEvent,
    },
};
use rust_decimal::Decimal;
use uuid::Uuid;

mod attempt_accounting;
mod query_contracts;
mod retention;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn operations_queries_pricing_rollups_health_and_completeness_reconcile() {
    let db = olp_db::test_support::TestDb::create_migrated("operations").await;
    let store = db.store(5).await;
    let owner = store
        .setup_installation(InstallationSetupInput {
            installation_name: "Operations integration".to_owned(),
            email: "owner@example.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash_password("correct horse battery staple").unwrap(),
        })
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
    .execute(store.pool())
    .await
    .unwrap();

    sqlx::query(
        "INSERT INTO api_keys
         (id, lookup_id, secret_digest, name, created_by)
         VALUES ($1, 'olpv2oper001', $2, 'operations test', $3)",
    )
    .bind(api_key_id)
    .bind([9_u8; 32].as_slice())
    .bind(owner.user_id)
    .execute(store.pool())
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
    .execute(store.pool())
    .await
    .unwrap();

    let fixture = query_contracts::exercise(
        &store,
        owner.user_id,
        provider_id,
        &master_key,
        api_key_id,
        generation_id,
    )
    .await;
    attempt_accounting::exercise(
        &store,
        owner.user_id,
        provider_id,
        api_key_id,
        generation_id,
    )
    .await;
    retention::exercise(&store, provider_id, api_key_id, generation_id, fixture).await;
}
