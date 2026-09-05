//! The configuration writes that used to run one statement per child row
//! (draft targets, discovered models, API key scopes and allowlists) now
//! validate with one query and insert with one `UNNEST`. These tests pin the
//! semantics that batching must not change: which invalid entry is reported,
//! that a rejection writes nothing, and that every child row still lands.

use crate::support::route_fixtures::{insert_provider, insert_unbased_route_draft};
use olp_db::{
    access::{Error as AccessError, NewApiKeyRecord},
    configuration::{
        error::Error,
        resources::{CapabilityRecord, DiscoveredModelInput, ReplaceRouteDraftInput},
        route_lifecycle::{NewRouteDraft, NewRouteTarget},
    },
    idempotency::{Outcome, Replayable, Response, fingerprint},
    identity::InstallationSetupInput,
    security::{envelope::MasterKey, key_material::AuthHmacKey, password::hash},
};
use olp_engine::domain::{
    auth::{ApiKeyLimits, ApiKeyScope},
    canonical::identity::{OperationKind, Surface, TransportMode},
    ids::RouteSlug,
    provider::CapabilitySource,
};
use uuid::Uuid;

async fn setup() -> (olp_db::test_support::TestDb, olp_db::store::Store, Uuid) {
    let db = olp_db::test_support::TestDb::create_migrated("batched_writes").await;
    let store = db.store(5).await;
    let owner = store
        .setup_installation(InstallationSetupInput {
            installation_name: "Batched writes".to_owned(),
            email: "owner@batched-writes.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash("correct horse battery staple").unwrap(),
        })
        .await
        .unwrap();
    (db, store, owner.user_id)
}

fn target(provider_id: Uuid, upstream_model: &str, weight: u32) -> NewRouteTarget {
    NewRouteTarget {
        provider_id,
        upstream_model: upstream_model.to_owned(),
        priority: 0,
        weight,
        timeout_ms: 20_000,
    }
}

async fn create_draft(
    store: &olp_db::store::Store,
    actor: Uuid,
    seed: &str,
    targets: Vec<NewRouteTarget>,
) -> Result<olp_db::configuration::route_lifecycle::RouteDraftCreated, Error> {
    let master_key = MasterKey::new(1, [37; 32]);
    let outcome = store
        .create_route_draft(
            NewRouteDraft {
                slug: seed.to_owned(),
                operations: vec![OperationKind::VideoGet, OperationKind::VideoCreate],
                overall_timeout_ms: 30_000,
                max_attempts: 2,
                targets,
                actor,
                idempotency_key: seed.to_owned(),
            },
            Replayable::new(fingerprint(&seed).unwrap(), &master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await?;
    match outcome {
        Outcome::Executed { value, .. } => Ok(value),
        Outcome::Replayed(_) => panic!("fresh draft creation replayed"),
    }
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn draft_targets_are_validated_in_position_order_and_written_together() {
    let (_db, store, actor) = setup().await;
    let alpha = insert_provider(store.pool(), actor, "batched-alpha").await;
    let beta = insert_provider(store.pool(), actor, "batched-beta").await;
    sqlx::query("UPDATE providers SET state = 'disabled'::provider_state WHERE id = $1")
        .bind(beta.provider_id)
        .execute(store.pool())
        .await
        .unwrap();

    // Position 1 is inactive and position 2 has an invalid weight: the
    // earlier one is reported, and nothing is written.
    let error = create_draft(
        &store,
        actor,
        "batched-inactive-first",
        vec![
            target(alpha.provider_id, "batched-alpha-model", 1),
            target(beta.provider_id, "batched-beta-model", 1),
            target(alpha.provider_id, "batched-alpha-model", 0),
        ],
    )
    .await
    .unwrap_err();
    assert!(matches!(
        &error,
        Error::InvalidRoute(message) if message.contains("is not active")
            && message.contains("batched-beta-model")
    ));
    let error = create_draft(
        &store,
        actor,
        "batched-weight-first",
        vec![
            target(alpha.provider_id, "batched-alpha-model", 0),
            target(beta.provider_id, "batched-beta-model", 1),
        ],
    )
    .await
    .unwrap_err();
    assert!(matches!(
        &error,
        Error::InvalidRoute(message) if message.contains("weight/timeout is invalid")
    ));
    let drafts: i64 = sqlx::query_scalar("SELECT count(*) FROM route_drafts")
        .fetch_one(store.pool())
        .await
        .unwrap();
    assert_eq!(drafts, 0);

    let created = create_draft(
        &store,
        actor,
        "batched-valid",
        vec![
            target(alpha.provider_id, " batched-alpha-model ", 2),
            target(alpha.provider_id, "batched-alpha-model", 1),
        ],
    )
    .await
    .unwrap();
    let draft = store.get_route_draft(created.id).await.unwrap();
    assert_eq!(
        draft.operations,
        vec![OperationKind::VideoCreate, OperationKind::VideoGet]
    );
    assert_eq!(
        draft
            .targets
            .iter()
            .map(|target| (target.position, target.weight))
            .collect::<Vec<_>>(),
        vec![(0, 2), (1, 1)]
    );

    // Replacement resolves every requested model in one query and still
    // names the first inactive one.
    let error = store
        .replace_route_draft(
            created.id,
            created.etag,
            &ReplaceRouteDraftInput {
                slug: "batched-valid".to_owned(),
                operations: vec![OperationKind::VideoGet],
                overall_timeout_ms: 30_000,
                max_attempts: 1,
                targets: vec![
                    (alpha.model_id, 0, 1, 20_000),
                    (beta.model_id, 0, 1, 20_000),
                ],
            },
            actor,
        )
        .await
        .unwrap_err();
    assert!(matches!(
        &error,
        Error::Invalid(message) if message.contains(&beta.model_id.to_string())
    ));
    store
        .replace_route_draft(
            created.id,
            created.etag,
            &ReplaceRouteDraftInput {
                slug: "batched-valid".to_owned(),
                operations: vec![OperationKind::VideoGet],
                overall_timeout_ms: 30_000,
                max_attempts: 1,
                targets: vec![
                    (alpha.model_id, 0, 3, 20_000),
                    (alpha.model_id, 1, 4, 20_000),
                ],
            },
            actor,
        )
        .await
        .unwrap();
    let draft = store.get_route_draft(created.id).await.unwrap();
    assert_eq!(draft.operations, vec![OperationKind::VideoGet]);
    assert_eq!(
        draft
            .targets
            .iter()
            .map(|target| (target.position, target.priority, target.weight))
            .collect::<Vec<_>>(),
        vec![(0, 0, 3), (1, 1, 4)]
    );
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn api_key_allowlist_is_checked_as_a_set_and_rejects_before_writing() {
    let (_db, store, actor) = setup().await;
    let provider = insert_provider(store.pool(), actor, "batched-key-provider").await;
    let draft =
        insert_unbased_route_draft(store.pool(), actor, "batched-live", &[provider.model_id]).await;
    let (etag, _) = store
        .validate_route_draft(draft.id, draft.etag, actor)
        .await
        .unwrap();
    store
        .activate_route_draft(draft.id, etag, actor, "batched-live-activate")
        .await
        .unwrap();

    let auth_hmac_key = AuthHmacKey::new([31; 32]);
    let master_key = MasterKey::new(1, [37; 32]);
    let key = |name: &str, allowed_routes: Vec<RouteSlug>| NewApiKeyRecord {
        name: name.to_owned(),
        material: auth_hmac_key.generate_api_key(),
        scopes: vec![ApiKeyScope::Inference, ApiKeyScope::ModelsRead],
        allowed_routes,
        limits: ApiKeyLimits::default(),
        expires_at: None,
        actor,
        idempotency_key: format!("batched-key-{name}"),
    };
    let rejected = key(
        "rejected",
        vec![
            RouteSlug::parse("batched-live").unwrap(),
            RouteSlug::parse("batched-missing").unwrap(),
        ],
    );
    let error = store
        .create_api_key_record(
            &rejected,
            Replayable::new(fingerprint(&rejected.idempotency_key).unwrap(), &master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap_err();
    assert!(matches!(
        &error,
        AccessError::Invalid(message) if message.contains("batched-missing")
    ));
    let rows: (i64, i64, i64) = sqlx::query_as(
        "SELECT (SELECT count(*) FROM api_keys), (SELECT count(*) FROM api_key_scopes), \
                (SELECT count(*) FROM api_key_route_allowlist)",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(rows, (0, 0, 0), "a rejected key must write nothing");

    let accepted = key("accepted", vec![RouteSlug::parse("batched-live").unwrap()]);
    store
        .create_api_key_record(
            &accepted,
            Replayable::new(fingerprint(&accepted.idempotency_key).unwrap(), &master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap();
    let scopes: i64 = sqlx::query_scalar("SELECT count(*) FROM api_key_scopes")
        .fetch_one(store.pool())
        .await
        .unwrap();
    let allowlisted: i64 = sqlx::query_scalar("SELECT count(*) FROM api_key_route_allowlist")
        .fetch_one(store.pool())
        .await
        .unwrap();
    assert_eq!((scopes, allowlisted), (2, 1));
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn discovered_models_and_capabilities_are_upserted_together() {
    let (_db, store, actor) = setup().await;
    let provider = insert_provider(store.pool(), actor, "batched-discovery").await;
    let etag: Uuid = sqlx::query_scalar("SELECT etag FROM providers WHERE id = $1")
        .bind(provider.provider_id)
        .fetch_one(store.pool())
        .await
        .unwrap();
    let capability = |operation: OperationKind, source: CapabilitySource| CapabilityRecord {
        operation,
        surface: Surface::OpenAi,
        mode: TransportMode::Unary,
        source,
        certified_at: None,
    };
    let models = vec![
        DiscoveredModelInput {
            // The existing fixture model: its row is updated, not duplicated.
            upstream_model: "batched-discovery-model".to_owned(),
            display_name: "Renamed".to_owned(),
            enabled: true,
            capabilities: vec![
                capability(OperationKind::Generation, CapabilitySource::Certified),
                capability(OperationKind::Embeddings, CapabilitySource::Declared),
            ],
        },
        DiscoveredModelInput {
            upstream_model: " brand-new ".to_owned(),
            display_name: "Brand new".to_owned(),
            enabled: false,
            capabilities: Vec::new(),
        },
    ];
    store
        .discover_provider_models(provider.provider_id, etag, &models, actor)
        .await
        .unwrap();

    let stored: Vec<(String, String, bool)> = sqlx::query_as(
        "SELECT upstream_model, display_name, enabled FROM provider_models \
         WHERE provider_id = $1 ORDER BY upstream_model",
    )
    .bind(provider.provider_id)
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert_eq!(
        stored,
        vec![
            (
                "batched-discovery-model".to_owned(),
                "Renamed".to_owned(),
                true
            ),
            ("brand-new".to_owned(), "Brand new".to_owned(), false),
        ]
    );
    let capabilities: Vec<(String, String, bool)> = sqlx::query_as(
        "SELECT mc.operation, mc.source, mc.certified_at IS NOT NULL \
         FROM model_capabilities mc JOIN provider_models pm ON pm.id = mc.provider_model_id \
         WHERE pm.provider_id = $1 ORDER BY mc.operation",
    )
    .bind(provider.provider_id)
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert_eq!(
        capabilities,
        vec![
            ("embeddings".to_owned(), "declared".to_owned(), false),
            ("generation".to_owned(), "certified".to_owned(), true),
        ]
    );
}
