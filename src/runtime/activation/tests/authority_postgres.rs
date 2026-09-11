use std::collections::BTreeSet;
use std::num::NonZeroU64;

use crate::access::identity::InstallationSetupInput;
use crate::access::policy::ApiKeyLimits;
use crate::access::policy::ApiKeyScope;
use crate::crypto::aad::credential;
use crate::crypto::session_material::SessionMaterial;
use crate::ids::ApiKeyLookupId;
use crate::test_support::TestDb;
use chrono::Duration;
use rust_decimal::Decimal;
use uuid::Uuid;

use crate::runtime::activation::tests::*;

struct Fixture {
    _database: TestDb,
    activator: RuntimeActivator,
    actor: Uuid,
    key_id: Uuid,
    lookup: ApiKeyLookupId,
}

async fn fixture() -> Fixture {
    let database = TestDb::create_migrated("authority").await;
    let pool = database.pool(5).await;
    let (owner, _) = crate::access::identity::setup::setup_installation_with_session(
        &pool,
        &crate::database::RequestProvenance::default(),
        InstallationSetupInput {
            installation_name: "Authority refresh".into(),
            email: "owner@authority.test".into(),
            display_name: "Owner".into(),
            password_hash: "synthetic-password-hash".into(),
        },
        &SessionMaterial::generate(),
        Duration::hours(1),
    )
    .await
    .unwrap();
    let key_id = Uuid::now_v7();
    let lookup = ApiKeyLookupId::parse("authority_refresh").unwrap();
    sqlx::query(
        "INSERT INTO api_keys (id, lookup_id, secret_digest, name, created_by, etag) \
         VALUES ($1, $2, $3, 'authority key', $4, $5)",
    )
    .bind(key_id)
    .bind(lookup.as_str())
    .bind(vec![1_u8; 32])
    .bind(owner.user_id)
    .bind(Uuid::now_v7())
    .execute(&pool)
    .await
    .unwrap();
    sqlx::query("INSERT INTO api_key_scopes (api_key_id, scope) VALUES ($1, 'inference')")
        .bind(key_id)
        .execute(&pool)
        .await
        .unwrap();
    crate::runtime::publication::compiler::compile_and_publish_runtime(&pool, owner.user_id)
        .await
        .unwrap();
    let mut activator = activator();
    activator.pool = pool;
    assert!(activator.activate().await.unwrap());
    Fixture {
        _database: database,
        activator,
        actor: owner.user_id,
        key_id,
        lookup,
    }
}

async fn corrupt_newest_release(fixture: &Fixture) -> i64 {
    let release = crate::runtime::publication::compiler::compile_and_publish_runtime(
        &fixture.activator.pool,
        fixture.actor,
    )
    .await
    .unwrap();
    sqlx::query("UPDATE runtime_generations SET release_sha256 = $1 WHERE id = $2")
        .bind(vec![0_u8; 32])
        .bind(release.generation_id)
        .execute(&fixture.activator.pool)
        .await
        .unwrap();
    release.sequence
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn current_policy_replaces_every_security_field_despite_a_corrupt_newer_release() {
    let fixture = fixture().await;
    let pinned = fixture.activator.runtime.pin();
    let pool = &fixture.activator.pool;
    let expiry = Utc::now() + Duration::hours(1);
    sqlx::query(
        "UPDATE api_keys SET secret_digest = $1, expires_at = $2, \
         requests_per_minute = 7, tokens_per_minute = 80, max_concurrency = 2, \
         daily_cost_limit = 1.23, monthly_cost_limit = 4.56 WHERE id = $3",
    )
    .bind(vec![2_u8; 32])
    .bind(expiry)
    .bind(fixture.key_id)
    .execute(pool)
    .await
    .unwrap();
    sqlx::query("UPDATE api_key_scopes SET scope = 'models_read' WHERE api_key_id = $1")
        .bind(fixture.key_id)
        .execute(pool)
        .await
        .unwrap();
    sqlx::query(
        "INSERT INTO api_key_route_allowlist (api_key_id, route_slug) VALUES ($1, 'restricted')",
    )
    .bind(fixture.key_id)
    .execute(pool)
    .await
    .unwrap();
    let rejected_sequence = corrupt_newest_release(&fixture).await;

    assert!(!fixture.activator.activate().await.unwrap());
    let refreshed = fixture.activator.runtime.pin();
    let key = &refreshed.api_keys[&fixture.lookup];
    assert_eq!(key.digest.as_bytes(), &[2; 32]);
    assert_eq!(
        key.expires_at.map(|value| value.timestamp_micros()),
        Some(expiry.timestamp_micros())
    );
    assert_eq!(key.scopes, BTreeSet::from([ApiKeyScope::ModelsRead]));
    assert_eq!(
        key.allowed_routes,
        BTreeSet::from([RouteSlug::parse("restricted").unwrap()])
    );
    assert_eq!(
        key.limits,
        ApiKeyLimits {
            requests_per_minute: NonZeroU32::new(7),
            tokens_per_minute: NonZeroU64::new(80),
            concurrency: NonZeroU32::new(2),
            daily_cost_limit: Some(Decimal::new(123, 2)),
            monthly_cost_limit: Some(Decimal::new(456, 2)),
        }
    );
    assert_eq!(refreshed.generation.id, pinned.generation.id);
    assert_eq!(refreshed.generation.ordinal, pinned.generation.ordinal);
    assert!(i64::try_from(refreshed.generation.ordinal).unwrap() < rejected_sequence);
    assert_eq!(
        fixture.activator.runtime.desired_generation_ordinal(),
        u64::try_from(rejected_sequence).unwrap()
    );
    assert_eq!(pinned.api_keys[&fixture.lookup].digest.as_bytes(), &[1; 32]);
    assert_eq!(
        pinned.api_keys[&fixture.lookup].scopes,
        BTreeSet::from([ApiKeyScope::Inference])
    );
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn rejected_generations_remain_desired_until_a_new_valid_release_converges() {
    for invalid_envelope in [false, true] {
        let fixture = fixture().await;
        let pool = &fixture.activator.pool;
        let active = fixture.activator.runtime.active_generation_ordinal();
        let rejected_sequence = corrupt_newest_release(&fixture).await;
        if invalid_envelope {
            sqlx::query(
                "UPDATE runtime_generations AS rejected \
                 SET compiled_release = valid.compiled_release, release_sha256 = valid.release_sha256 \
                 FROM runtime_generations AS valid \
                 WHERE rejected.sequence = $1 AND valid.sequence = $2",
            )
            .bind(rejected_sequence)
            .bind(i64::try_from(active.unwrap()).unwrap())
            .execute(pool)
            .await
            .unwrap();
        }
        let desired = u64::try_from(rejected_sequence).unwrap();
        let mut restarted = activator();
        restarted.pool = pool.clone();

        assert!(!fixture.activator.activate().await.unwrap());
        assert!(restarted.activate().await.unwrap());
        for activator in [&fixture.activator, &restarted] {
            assert_eq!(activator.runtime.active_generation_ordinal(), active);
            assert_eq!(activator.runtime.desired_generation_ordinal(), desired);
            assert!(!activator.activate().await.unwrap());
            assert_eq!(activator.runtime.active_generation_ordinal(), active);
            assert_eq!(activator.runtime.desired_generation_ordinal(), desired);
        }

        let recovered =
            crate::runtime::publication::compiler::compile_and_publish_runtime(pool, fixture.actor)
                .await
                .unwrap();
        let recovered_sequence = u64::try_from(recovered.sequence).unwrap();
        assert!(recovered_sequence > desired);
        for activator in [&fixture.activator, &restarted] {
            assert!(activator.activate().await.unwrap());
            assert_eq!(
                activator.runtime.active_generation_ordinal(),
                Some(recovered_sequence)
            );
            assert_eq!(
                activator.runtime.desired_generation_ordinal(),
                recovered_sequence
            );
        }
    }
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn revocation_and_expiry_apply_without_any_new_routing_release() {
    let fixture = fixture().await;
    let generation = fixture.activator.runtime.active_generation_ordinal();
    let pool = &fixture.activator.pool;
    sqlx::query("UPDATE api_keys SET revoked_at = now() WHERE id = $1")
        .bind(fixture.key_id)
        .execute(pool)
        .await
        .unwrap();
    assert!(!fixture.activator.activate().await.unwrap());
    assert!(fixture.activator.runtime.pin().api_keys.is_empty());

    sqlx::query("UPDATE api_keys SET revoked_at = NULL WHERE id = $1")
        .bind(fixture.key_id)
        .execute(pool)
        .await
        .unwrap();
    assert!(!fixture.activator.activate().await.unwrap());
    assert!(
        fixture
            .activator
            .runtime
            .pin()
            .api_keys
            .contains_key(&fixture.lookup)
    );
    sqlx::query("UPDATE api_keys SET expires_at = now() - interval '1 hour' WHERE id = $1")
        .bind(fixture.key_id)
        .execute(pool)
        .await
        .unwrap();
    assert!(!fixture.activator.activate().await.unwrap());
    assert!(fixture.activator.runtime.pin().api_keys.is_empty());
    assert_eq!(
        fixture.activator.runtime.active_generation_ordinal(),
        generation
    );
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn a_failed_authority_poll_keeps_the_last_successfully_refreshed_bundle() {
    let fixture = fixture().await;
    sqlx::query("UPDATE api_keys SET secret_digest = $1 WHERE id = $2")
        .bind(vec![2_u8; 32])
        .bind(fixture.key_id)
        .execute(&fixture.activator.pool)
        .await
        .unwrap();
    assert!(!fixture.activator.activate().await.unwrap());
    let refreshed = fixture.activator.runtime.pin();
    fixture.activator.pool.close().await;

    assert!(fixture.activator.activate().await.is_err());
    assert!(Arc::ptr_eq(&refreshed, &fixture.activator.runtime.pin()));
    assert_eq!(
        refreshed.api_keys[&fixture.lookup].digest.as_bytes(),
        &[2; 32]
    );
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn an_older_authority_read_cannot_finish_after_a_newer_policy_poll() {
    let fixture = fixture().await;
    let mut first = fixture.activator.clone();
    let second = fixture.activator.clone();
    let barrier = Arc::new(Barrier::new(2));
    first.after_authority_read = Some(barrier.clone());
    let older = tokio::spawn(async move { first.activate().await });
    barrier.wait().await;
    sqlx::query("UPDATE api_keys SET secret_digest = $1 WHERE id = $2")
        .bind(vec![2_u8; 32])
        .bind(fixture.key_id)
        .execute(&fixture.activator.pool)
        .await
        .unwrap();
    assert!(second.activation_lock.try_lock().is_err());
    let mut newer = Box::pin(second.activate());
    assert!(futures::poll!(&mut newer).is_pending());

    barrier.wait().await;
    assert!(!older.await.unwrap().unwrap());
    assert!(!newer.await.unwrap());
    assert_eq!(
        fixture.activator.runtime.pin().api_keys[&fixture.lookup]
            .digest
            .as_bytes(),
        &[2; 32]
    );
}

async fn add_provider(fixture: &mut Fixture) -> ProviderId {
    let provider_id = Uuid::now_v7();
    let credential_id = Uuid::now_v7();
    let revision_id = Uuid::now_v7();
    let master_key = Arc::new(MasterKey::new(1, [3; 32]));
    let encrypted = master_key
        .seal(
            b"synthetic-provider-key",
            &credential(provider_id, credential_id, 1),
        )
        .unwrap();
    let pool = &fixture.activator.pool;
    sqlx::query(
        "INSERT INTO providers (id, name, kind, state, endpoint, auth_mode, etag, created_by) \
         VALUES ($1, 'retained-provider', 'openai_compatible', 'active'::provider_state, \
         'https://provider.example.test/v1/', 'api_key', $2, $3)",
    )
    .bind(provider_id)
    .bind(Uuid::now_v7())
    .bind(fixture.actor)
    .execute(pool)
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO provider_credential_versions \
         (id, provider_id, version, ciphertext, nonce, master_key_version, created_by) \
         VALUES ($1, $2, 1, $3, $4, 1, $5)",
    )
    .bind(credential_id)
    .bind(provider_id)
    .bind(encrypted.ciphertext)
    .bind(encrypted.nonce.to_vec())
    .bind(fixture.actor)
    .execute(pool)
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO provider_revisions \
         (id, provider_id, revision, name, kind, endpoint, auth_mode, connector_ready, \
         credential_version_id, source_etag, activated_by) \
         SELECT $1, id, 1, name, kind, endpoint, auth_mode, connector_ready, $2, etag, $3 \
         FROM providers WHERE id = $4",
    )
    .bind(revision_id)
    .bind(credential_id)
    .bind(fixture.actor)
    .bind(provider_id)
    .execute(pool)
    .await
    .unwrap();
    sqlx::query("UPDATE providers SET active_revision_id = $1, active_credential_version_id = $2 WHERE id = $3")
        .bind(revision_id).bind(credential_id).bind(provider_id)
        .execute(pool).await.unwrap();
    let mut transaction = pool.begin().await.unwrap();
    crate::providers::pool_store::snapshot(&mut transaction, provider_id, revision_id)
        .await
        .unwrap();
    transaction.commit().await.unwrap();
    crate::runtime::publication::compiler::compile_and_publish_runtime(pool, fixture.actor)
        .await
        .unwrap();
    fixture.activator.master_key = Some(master_key);
    assert!(fixture.activator.activate().await.unwrap());
    ProviderId::from_uuid(provider_id)
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make integration"]
async fn mounted_transports_enforce_pool_limits_and_reject_named_slots() {
    use crate::protocols::canonical::identity::{
        OperationKind, RequestMetadata, Surface, TransportMode,
    };
    use crate::protocols::canonical::requests::Operation;
    use crate::routes::selection::AttemptPlan;

    let mut fixture = fixture().await;
    let provider_id = add_provider(&mut fixture).await;
    let master_key = fixture.activator.master_key.take().unwrap();
    fixture
        .activator
        .transports
        .register(provider_id, Arc::new(UnusedTransport));
    let pool = &fixture.activator.pool;
    let pinned = fixture.activator.runtime.pin();
    let provider = &pinned.providers[&provider_id];
    let credential_id = provider.active_credential.unwrap().as_uuid();
    for connection_limit in [true, false] {
        let options = if connection_limit {
            serde_json::json!({"limits":{"requests_per_minute":1}})
        } else {
            serde_json::json!({})
        };
        sqlx::query("UPDATE provider_revisions SET options=$2 WHERE id=$1")
            .bind(provider.revision_id)
            .bind(options)
            .execute(pool)
            .await
            .unwrap();
        sqlx::query("UPDATE provider_revision_credentials SET configuration=jsonb_set(configuration,'{requests_per_minute}',$2) WHERE provider_revision_id=$1")
            .bind(provider.revision_id)
            .bind(if connection_limit { serde_json::Value::Null } else { serde_json::json!(1) })
            .execute(pool).await.unwrap();
        crate::runtime::publication::compiler::compile_and_publish_runtime(pool, fixture.actor)
            .await
            .unwrap();
        assert!(fixture.activator.activate().await.unwrap());
        let active = fixture.activator.runtime.pin();
        let request = ProviderRequest {
            metadata: RequestMetadata {
                request_id: crate::ids::RequestId::new(),
                operation: OperationKind::Generation,
                surface: Surface::OpenAi,
                mode: TransportMode::Unary,
            },
            attempt: AttemptPlan {
                connection_limits: None,
                credential_limits: None,
                attempt_limit: None,
                routing_policy: None,
                credential_slot_id: Some(provider_id.as_uuid()),
                credential_version_id: Some(credential_id),
                pricing_revision_id: None,
                generation_id: active.generation.id,
                route_id: RouteId::new(),
                target_id: TargetId::new(),
                routing_id: TargetId::new(),
                provider_id,
                provider_revision_id: provider.revision_id,
                provider_kind: provider.kind,
                upstream_model: "mounted-model".into(),
                timeout: DurationMs::new(1000),
                priority: 0,
            },
            operation: Arc::new(Operation::Generation(
                crate::providers::openai::certification::probe_generation_request(
                    TransportMode::Unary,
                    Default::default(),
                ),
            )),
            media: None,
            max_inline_media_bytes: 1024,
            propagate_trace_context: false,
        };
        let error = active
            .transport(provider_id)
            .unwrap()
            .execute(request)
            .await
            .unwrap_err();
        assert_eq!(error.message, "provider_limits_unavailable");
    }

    let slot = Uuid::now_v7();
    let named_secret = Uuid::now_v7();
    let encrypted = master_key
        .seal(
            b"mounted-named-secret",
            &credential(provider_id.as_uuid(), named_secret, 2),
        )
        .unwrap();
    sqlx::query("INSERT INTO provider_credential_slots(id,provider_id,name) VALUES($1,$2,'Named')")
        .bind(slot)
        .bind(provider_id.as_uuid())
        .execute(pool)
        .await
        .unwrap();
    sqlx::query("INSERT INTO provider_credential_versions(id,provider_id,slot_id,version,ciphertext,nonce,master_key_version,created_by) VALUES($1,$2,$3,2,$4,$5,1,$6)")
        .bind(named_secret).bind(provider_id.as_uuid()).bind(slot)
        .bind(encrypted.ciphertext).bind(encrypted.nonce.to_vec()).bind(fixture.actor)
        .execute(pool).await.unwrap();
    sqlx::query("INSERT INTO provider_revision_credentials(provider_revision_id,slot_id,credential_version_id,configuration) VALUES($1,$2,$3,$4)")
        .bind(provider.revision_id).bind(slot).bind(named_secret)
        .bind(sqlx::types::Json(crate::providers::pool::CredentialSlot {
            id: slot,
            name: "Named".into(),
            credential_version_id: Some(named_secret),
            ..Default::default()
        })).execute(pool).await.unwrap();
    let previous = fixture.activator.runtime.active_generation_ordinal();
    crate::runtime::publication::compiler::compile_and_publish_runtime(pool, fixture.actor)
        .await
        .unwrap();
    let error = fixture.activator.activate().await.unwrap_err().to_string();
    assert!(
        error.contains("OLP_MASTER_KEY_FILE is required for named credential slots"),
        "{error}"
    );
    assert_eq!(
        fixture.activator.runtime.active_generation_ordinal(),
        previous
    );
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn revoked_authority_applies_when_a_new_transport_cannot_be_constructed() {
    let mut fixture = fixture().await;
    let provider = add_provider(&mut fixture).await;
    let pinned = fixture.activator.runtime.pin();
    let pool = &fixture.activator.pool;
    let rejected =
        crate::runtime::publication::compiler::compile_and_publish_runtime(pool, fixture.actor)
            .await
            .unwrap();
    sqlx::query("UPDATE runtime_generation_provider_configs SET endpoint = 'not-a-url' WHERE runtime_generation_id = $1")
        .bind(rejected.generation_id).execute(pool).await.unwrap();
    sqlx::query("UPDATE api_keys SET revoked_at = now() WHERE id = $1")
        .bind(fixture.key_id)
        .execute(pool)
        .await
        .unwrap();
    assert_eq!(
        crate::runtime::publication::releases::valid_runtime_release(pool, rejected.generation_id)
            .await
            .unwrap()
            .sequence,
        rejected.sequence
    );
    assert!(
        crate::providers::runtime::runtime_provider_configurations(
            pool,
            &Manager::decode_persisted_release(&rejected.activation_candidate()).unwrap()
        )
        .await
        .is_ok()
    );

    let error = fixture.activator.activate().await.unwrap_err().to_string();
    assert!(
        error.contains("custom OpenAI endpoint URL is invalid"),
        "{error}"
    );
    let refreshed = fixture.activator.runtime.pin();
    assert!(refreshed.api_keys.is_empty());
    assert!(pinned.api_keys.contains_key(&fixture.lookup));
    assert_eq!(refreshed.generation.id, pinned.generation.id);
    assert_eq!(refreshed.generation.ordinal, pinned.generation.ordinal);
    assert!(Arc::ptr_eq(
        &refreshed.transport(provider).unwrap(),
        &pinned.transport(provider).unwrap()
    ));
}
