use chrono::{Duration, Utc};
use olp_db::{
    access::NewApiKeyRecord, configuration::Error, configuration::NewProviderDraft,
    configuration::NewRouteDraft, configuration::NewRouteTarget,
    configuration::resources::CapabilityCertificationOutcome,
    configuration::resources::CapabilityRecord, configuration::resources::DiscoveredModelInput,
    configuration::resources::ProviderModelRecord,
    configuration::resources::ReplaceRouteDraftInput, configuration::resources::RotateApiKeyInput,
    configuration::resources::RotateCredentialInput, configuration::resources::UpdateApiKeyInput,
    configuration::resources::UpdateProvider, error::Error as PersistenceError,
    idempotency::Outcome, idempotency::Replayable, idempotency::Response, idempotency::fingerprint,
    identity::InstallationSetupInput, security::aad::credential, security::envelope::MasterKey,
    security::key_material::AuthHmacKey, security::password::hash,
    security::session_material::SessionMaterial, store::Store,
};
use olp_engine::domain::{
    auth::{ApiKeyLimits, ApiKeyScope},
    canonical::identity::OperationKind,
    ids::{CredentialVersionId, ProviderId, RouteSlug},
    routing::{provider::ProviderKind, snapshot::Snapshot},
};
use uuid::Uuid;

mod disabled_guards;
mod eligibility;
mod lifecycle;

trait ExpectExecuted<T> {
    fn expect_executed(self) -> T;
}

impl<T> ExpectExecuted<T> for Outcome<T> {
    fn expect_executed(self) -> T {
        match self {
            Outcome::Executed { value, .. } => value,
            Outcome::Replayed(_) => panic!("fresh integration operation replayed"),
        }
    }
}

fn test_replay<'a>(master_key: &'a MasterKey, seed: &str) -> Replayable<'a> {
    Replayable::new(fingerprint(&seed).unwrap(), master_key)
}

fn empty_created_response<T>(_: &T) -> Result<Response, PersistenceError> {
    Response::new(201, None, None, Vec::new())
}

async fn provider_models(store: &Store, provider_id: Uuid) -> Vec<ProviderModelRecord> {
    let page = store
        .list_provider_models(provider_id, None, 100)
        .await
        .unwrap();
    assert!(page.next_cursor.is_none());
    page.items
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn configuration_lifecycle_is_versioned_audited_and_publishes_runtime() {
    let db = olp_db::test_support::TestDb::create_migrated("configuration").await;
    let store = db.store(5).await;
    let session = SessionMaterial::generate();
    let (owner, _) = store
        .setup_installation_with_session(
            InstallationSetupInput {
                installation_name: "Configuration integration".to_owned(),
                email: "owner@configuration.test".to_owned(),
                display_name: "Owner".to_owned(),
                password_hash: hash("correct horse battery staple").unwrap(),
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

async fn certify_all_capabilities(store: &Store, provider_id: Uuid) {
    sqlx::query(
        "UPDATE model_capabilities SET source = 'certified', certified_at = now() \
         WHERE provider_model_id IN (SELECT id FROM provider_models WHERE provider_id = $1)",
    )
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
}
