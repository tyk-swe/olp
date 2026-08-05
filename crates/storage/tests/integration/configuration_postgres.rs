use chrono::{Duration, Utc};
use olp_domain::{
    ApiKeyLimits, ApiKeyScope, CredentialVersionId, OperationKind, ProviderId, ProviderKind,
    RouteSlug, RuntimeSnapshot,
};
use olp_storage::{
    PersistenceError, PgStore, access::NewApiKeyRecord,
    configuration::CapabilityCertificationOutcome, configuration::CapabilityRecord,
    configuration::ConfigurationError, configuration::DiscoveredModelInput,
    configuration::NewProviderDraft, configuration::NewRouteDraft, configuration::NewRouteTarget,
    configuration::ProviderModelRecord, configuration::ReplaceRouteDraftInput,
    configuration::RotateApiKeyInput, configuration::RotateCredentialInput,
    configuration::UpdateApiKeyInput, configuration::UpdateProvider,
    idempotency::IdempotencyOutcome, idempotency::IdempotencyResponse,
    idempotency::ReplayableIdempotency, idempotency::idempotency_fingerprint,
    identity::InstallationSetupInput, security::AuthHmacKey, security::MasterKey,
    security::SessionMaterial, security::credential_aad, security::hash_password,
};
use uuid::Uuid;

mod eligibility;
mod lifecycle;

trait ExpectExecuted<T> {
    fn expect_executed(self) -> T;
}

impl<T> ExpectExecuted<T> for IdempotencyOutcome<T> {
    fn expect_executed(self) -> T {
        match self {
            IdempotencyOutcome::Executed { value, .. } => value,
            IdempotencyOutcome::Replayed(_) => panic!("fresh integration operation replayed"),
        }
    }
}

fn test_replay<'a>(master_key: &'a MasterKey, seed: &str) -> ReplayableIdempotency<'a> {
    ReplayableIdempotency::new(idempotency_fingerprint(&seed).unwrap(), master_key)
}

fn empty_created_response<T>(_: &T) -> Result<IdempotencyResponse, PersistenceError> {
    IdempotencyResponse::new(201, None, None, Vec::new())
}

async fn provider_models(store: &PgStore, provider_id: Uuid) -> Vec<ProviderModelRecord> {
    let page = store
        .list_provider_models(provider_id, None, 100)
        .await
        .unwrap();
    assert!(page.next_cursor.is_none());
    page.items
}

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn configuration_lifecycle_is_versioned_audited_and_publishes_runtime() {
    let db = olp_storage::test_support::TestDb::create_migrated("configuration").await;
    let store = db.store(5).await;
    let session = SessionMaterial::generate();
    let (owner, _) = store
        .setup_installation_with_session(
            InstallationSetupInput {
                installation_name: "Configuration integration".to_owned(),
                email: "owner@configuration.test".to_owned(),
                display_name: "Owner".to_owned(),
                password_hash: hash_password("correct horse battery staple").unwrap(),
            },
            &session,
            chrono::Duration::hours(12),
        )
        .await
        .unwrap();
    let actor = owner.user_id;
    let master_key = MasterKey::new(1, [7; 32]);
    let (provider_id, revoked_etag) = lifecycle::exercise(&store, actor, &master_key).await;
    eligibility::exercise(&store, actor, &master_key, provider_id, revoked_etag).await;
}

async fn certify_all_capabilities(store: &PgStore, provider_id: Uuid) {
    sqlx::query(
        "UPDATE model_capabilities SET source = 'certified', certified_at = now() \
         WHERE provider_model_id IN (SELECT id FROM provider_models WHERE provider_id = $1)",
    )
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
}
