use chrono::Duration;
use chrono::Utc;
use olp::access::api_keys::lifecycle::NewApiKeyRecord;
use olp::access::api_keys::records::RotateApiKeyInput;
use olp::access::api_keys::records::UpdateApiKeyInput;
use olp::access::identity::InstallationSetupInput;
use olp::access::policy::ApiKeyLimits;
use olp::access::policy::ApiKeyScope;
use olp::crypto::aad::credential;
use olp::crypto::envelope::MasterKey;
use olp::crypto::key_material::AuthHmacKey;
use olp::crypto::password::hash;
use olp::crypto::session_material::SessionMaterial;
use olp::database::error::Error as PersistenceError;
use olp::database::idempotency::Outcome;
use olp::database::idempotency::Replayable;
use olp::database::idempotency::Response;
use olp::database::idempotency::fingerprint;
use olp::ids::CredentialVersionId;
use olp::ids::ProviderId;
use olp::ids::RouteSlug;
use olp::protocols::canonical::identity::OperationKind;
use olp::providers::error::Error;
use olp::providers::lifecycle::NewProviderDraft;
use olp::providers::records::CapabilityCertificationOutcome;
use olp::providers::records::CapabilityRecord;
use olp::providers::records::DiscoveredModelInput;
use olp::providers::records::ProviderModelRecord;
use olp::providers::records::RotateCredentialInput;
use olp::providers::records::UpdateProvider;
use olp::providers::runtime_model::ProviderKind;
use olp::routes::drafts::NewRouteDraft;
use olp::routes::drafts::NewRouteTarget;
use olp::routes::records::ReplaceRouteDraftInput;
use olp::runtime::snapshot::Snapshot;
use rust_decimal::Decimal;
use sqlx::PgPool;
use uuid::Uuid;

mod api_key_issuers;
mod disabled_guards;
mod eligibility;
mod lifecycle;
mod model_snapshot;

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

async fn provider_models(pool: &PgPool, provider_id: Uuid) -> Vec<ProviderModelRecord> {
    let page = olp::providers::models::list_provider_models(pool, provider_id, None, 100)
        .await
        .unwrap();
    assert!(page.next_cursor.is_none());
    page.items
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn configuration_lifecycle_is_versioned_audited_and_publishes_runtime() {
    let db = olp::test_support::TestDb::create_migrated("configuration").await;
    let pool = db.pool(5).await;
    let session = SessionMaterial::generate();
    let (owner, _) = olp::access::identity::setup::setup_installation_with_session(
        &pool,
        &olp::database::RequestProvenance::default(),
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
    let (provider_id, revoked_etag) = lifecycle::exercise(&pool, actor, &master_key).await;
    eligibility::exercise(&pool, actor, &master_key, provider_id, revoked_etag).await;
}

async fn certify_all_capabilities(pool: &PgPool, provider_id: Uuid) {
    sqlx::query(
        "UPDATE model_capabilities SET source = 'certified', certified_at = now() \
         WHERE provider_model_id IN (SELECT id FROM provider_models WHERE provider_id = $1)",
    )
    .bind(provider_id)
    .execute(pool)
    .await
    .unwrap();
}
