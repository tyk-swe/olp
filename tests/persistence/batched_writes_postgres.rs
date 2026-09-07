//! The configuration writes that used to run one statement per child row
//! (draft targets, discovered models, API key scopes and allowlists) now
//! validate with one query and insert with one `UNNEST`. These tests pin the
//! semantics that batching must not change: which invalid entry is reported,
//! that a rejection writes nothing, and that every child row still lands.

use crate::support::route_fixtures::insert_provider;
use crate::support::route_fixtures::insert_unbased_route_draft;
use olp::access::api_keys::lifecycle::Error as AccessError;
use olp::access::api_keys::lifecycle::NewApiKeyRecord;
use olp::access::identity::InstallationSetupInput;
use olp::access::policy::ApiKeyLimits;
use olp::access::policy::ApiKeyScope;
use olp::crypto::envelope::MasterKey;
use olp::crypto::key_material::AuthHmacKey;
use olp::crypto::password::hash;
use olp::database::idempotency::Outcome;
use olp::database::idempotency::Replayable;
use olp::database::idempotency::Response;
use olp::database::idempotency::fingerprint;
use olp::ids::RouteSlug;
use olp::protocols::canonical::identity::OperationKind;
use olp::protocols::canonical::identity::Surface;
use olp::protocols::canonical::identity::TransportMode;
use olp::providers::error::Error;
use olp::providers::records::CapabilityRecord;
use olp::providers::records::DiscoveredModelInput;
use olp::providers::types::CapabilitySource;
use olp::routes::drafts::NewRouteDraft;
use olp::routes::drafts::NewRouteTarget;
use olp::routes::records::ReplaceRouteDraftInput;
use uuid::Uuid;

async fn setup() -> (olp::test_support::TestDb, sqlx::PgPool, Uuid) {
    let db = olp::test_support::TestDb::create_migrated("batched_writes").await;
    let pool = db.pool(5).await;
    let owner = olp::access::identity::setup::setup_installation(
        &pool,
        &olp::database::RequestProvenance::default(),
        InstallationSetupInput {
            installation_name: "Batched writes".to_owned(),
            email: "owner@batched-writes.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash("correct horse battery staple").unwrap(),
        },
    )
    .await
    .unwrap();
    (db, pool, owner.user_id)
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
    pool: &sqlx::PgPool,
    actor: Uuid,
    seed: &str,
    targets: Vec<NewRouteTarget>,
) -> Result<olp::routes::drafts::RouteDraftCreated, Error> {
    let master_key = MasterKey::new(1, [37; 32]);
    let outcome = olp::routes::drafts::create_route_draft(
        pool,
        &olp::database::RequestProvenance::default(),
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
    let (_db, pool, actor) = setup().await;
    let alpha = insert_provider(&pool, actor, "batched-alpha").await;
    let beta = insert_provider(&pool, actor, "batched-beta").await;
    sqlx::query("UPDATE providers SET state = 'disabled'::provider_state WHERE id = $1")
        .bind(beta.provider_id)
        .execute(&pool)
        .await
        .unwrap();

    // Position 1 is inactive and position 2 has an invalid weight: the
    // earlier one is reported, and nothing is written.
    let error = create_draft(
        &pool,
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
        &pool,
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
        .fetch_one(&pool)
        .await
        .unwrap();
    assert_eq!(drafts, 0);

    let created = create_draft(
        &pool,
        actor,
        "batched-valid",
        vec![
            target(alpha.provider_id, " batched-alpha-model ", 2),
            target(alpha.provider_id, "batched-alpha-model", 1),
        ],
    )
    .await
    .unwrap();
    let draft = olp::routes::repository::get_route_draft(&pool, created.id)
        .await
        .unwrap();
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
    let error = olp::routes::repository::replace_route_draft(
        &pool,
        &olp::database::RequestProvenance::default(),
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
    olp::routes::repository::replace_route_draft(
        &pool,
        &olp::database::RequestProvenance::default(),
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
    let draft = olp::routes::repository::get_route_draft(&pool, created.id)
        .await
        .unwrap();
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
    let (_db, pool, actor) = setup().await;
    let provider = insert_provider(&pool, actor, "batched-key-provider").await;
    let draft =
        insert_unbased_route_draft(&pool, actor, "batched-live", &[provider.model_id]).await;
    let (etag, _) = olp::routes::drafts::validate_route_draft(
        &pool,
        &olp::database::RequestProvenance::default(),
        draft.id,
        draft.etag,
        actor,
    )
    .await
    .unwrap();
    olp::routes::drafts::activate_route_draft(
        &pool,
        &olp::database::RequestProvenance::default(),
        draft.id,
        etag,
        actor,
        "batched-live-activate",
    )
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
    let error = olp::access::api_keys::lifecycle::create_api_key_record(
        &pool,
        &olp::database::RequestProvenance::default(),
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
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(rows, (0, 0, 0), "a rejected key must write nothing");

    let accepted = key("accepted", vec![RouteSlug::parse("batched-live").unwrap()]);
    olp::access::api_keys::lifecycle::create_api_key_record(
        &pool,
        &olp::database::RequestProvenance::default(),
        &accepted,
        Replayable::new(fingerprint(&accepted.idempotency_key).unwrap(), &master_key),
        |_| Response::new(201, None, None, Vec::new()),
    )
    .await
    .unwrap();
    let scopes: i64 = sqlx::query_scalar("SELECT count(*) FROM api_key_scopes")
        .fetch_one(&pool)
        .await
        .unwrap();
    let allowlisted: i64 = sqlx::query_scalar("SELECT count(*) FROM api_key_route_allowlist")
        .fetch_one(&pool)
        .await
        .unwrap();
    assert_eq!((scopes, allowlisted), (2, 1));
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn discovered_models_and_capabilities_are_upserted_together() {
    let (_db, pool, actor) = setup().await;
    let provider = insert_provider(&pool, actor, "batched-discovery").await;
    let etag: Uuid = sqlx::query_scalar("SELECT etag FROM providers WHERE id = $1")
        .bind(provider.provider_id)
        .fetch_one(&pool)
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
    olp::providers::models::discover_provider_models(
        &pool,
        &olp::database::RequestProvenance::default(),
        provider.provider_id,
        etag,
        &models,
        actor,
    )
    .await
    .unwrap();

    let stored: Vec<(String, String, bool)> = sqlx::query_as(
        "SELECT upstream_model, display_name, enabled FROM provider_models \
         WHERE provider_id = $1 ORDER BY upstream_model",
    )
    .bind(provider.provider_id)
    .fetch_all(&pool)
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
    .fetch_all(&pool)
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
