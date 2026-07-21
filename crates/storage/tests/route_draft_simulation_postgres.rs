#[path = "support/route_fixtures.rs"]
mod route_fixtures;

use olp_domain::{
    OperationKind, RouteSlug, RuntimeSnapshot, Surface, TransportMode, select_attempts,
};
use olp_storage::{InstallationSetupInput, PgStore, ReplaceRouteDraftInput, SessionMaterial};
use route_fixtures::{DraftFixture, insert_provider};
use sqlx::{PgPool, Row};
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn route_draft_simulation_matches_activated_runtime_attempts() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 5).await.unwrap();
    store.migrate().await.unwrap();
    let (owner, _) = store
        .setup_installation_with_session(
            InstallationSetupInput {
                installation_name: "Route simulation".to_owned(),
                email: "owner@route-simulation.test".to_owned(),
                display_name: "Owner".to_owned(),
                password_hash: "test-password-hash".to_owned(),
            },
            &SessionMaterial::generate(),
            chrono::Duration::hours(1),
        )
        .await
        .unwrap();
    let actor = owner.user_id;
    let primary = insert_provider(store.pool(), actor, "simulation-primary").await;
    let fallback = insert_provider(store.pool(), actor, "simulation-fallback").await;
    let draft = insert_unbased_route_draft(
        store.pool(),
        actor,
        "simulation",
        &[primary.model_id, fallback.model_id],
    )
    .await;
    let initial_targets = store.get_route_draft(draft.id).await.unwrap().targets;
    let replacement_etag = store
        .replace_route_draft(
            draft.id,
            draft.etag,
            &ReplaceRouteDraftInput {
                slug: "simulation".to_owned(),
                operations: vec!["video_get".parse().unwrap()],
                overall_timeout_ms: 30_000,
                max_attempts: 2,
                targets: vec![
                    (fallback.model_id, 0, 1, 20_000),
                    (primary.model_id, 0, 1, 20_000),
                ],
            },
            actor,
        )
        .await
        .unwrap();
    let draft = store.get_route_draft(draft.id).await.unwrap();
    assert!(
        draft
            .targets
            .iter()
            .all(|target| !initial_targets.iter().any(|old| old.id == target.id))
    );
    assert!(draft.targets.iter().all(|target| {
        !initial_targets
            .iter()
            .any(|old| old.routing_id == target.routing_id)
    }));

    let seed = "route-draft-simulation-affinity";
    let simulation = store
        .simulate_route_draft(
            draft.id,
            "video_get".parse().unwrap(),
            "openai".parse().unwrap(),
            "unary".parse().unwrap(),
            seed,
        )
        .await
        .unwrap();
    let simulated_routing_ids = simulation
        .targets
        .iter()
        .filter_map(|target| {
            target.attempt.map(|_| {
                draft
                    .targets
                    .iter()
                    .find(|draft_target| draft_target.id == target.target_id)
                    .unwrap()
                    .routing_id
            })
        })
        .collect::<Vec<_>>();
    assert_eq!(simulated_routing_ids.len(), 2);

    // A competing activation can claim the slug after simulation. The draft
    // identity must still produce the simulated order when this activation
    // attaches its revision to that competing route.
    let conflicting_route_id = Uuid::now_v7();
    sqlx::query("INSERT INTO routes (id, slug, created_by) VALUES ($1, 'simulation', $2)")
        .bind(conflicting_route_id)
        .bind(actor)
        .execute(store.pool())
        .await
        .unwrap();

    let (validated_etag, _) = store
        .validate_route_draft(draft.id, replacement_etag, actor)
        .await
        .unwrap();
    let first_activation = store
        .activate_route_draft(draft.id, validated_etag, actor, "route-simulation-activate")
        .await
        .unwrap();
    assert_eq!(first_activation.route_id, conflicting_route_id);
    assert_ne!(first_activation.route_id, draft.id);
    let second_activation = store
        .activate_route_draft(
            draft.id,
            validated_etag,
            actor,
            "route-simulation-activate-repeat",
        )
        .await
        .unwrap();
    assert_eq!(second_activation.route_id, first_activation.route_id);
    assert_ne!(second_activation.revision_id, first_activation.revision_id);

    let first_targets =
        revision_target_identities(store.pool(), first_activation.revision_id).await;
    let second_targets =
        revision_target_identities(store.pool(), second_activation.revision_id).await;
    assert_eq!(first_targets.len(), second_targets.len());
    assert!(first_targets.iter().zip(&second_targets).all(
        |((first_id, first_routing_id), (second_id, second_routing_id))| {
            first_id != second_id && first_routing_id == second_routing_id
        }
    ));

    let compiled = store.compile_and_publish_runtime(actor).await.unwrap();
    let runtime: RuntimeSnapshot = serde_json::from_slice(&compiled.payload).unwrap();
    let route_slug = RouteSlug::parse("simulation").unwrap();
    let route = runtime.routes.get(&route_slug).unwrap();
    assert_eq!(route.id.as_uuid(), second_activation.route_id);
    assert_eq!(
        route.routing_id.map(|routing_id| routing_id.as_uuid()),
        Some(draft.routing_id)
    );
    assert_eq!(
        route
            .targets
            .iter()
            .map(|target| target.id.as_uuid())
            .collect::<Vec<_>>(),
        second_targets
            .iter()
            .map(|(target_id, _)| *target_id)
            .collect::<Vec<_>>()
    );
    let runtime_routing_ids = select_attempts(
        &runtime,
        &route_slug,
        OperationKind::VideoGet,
        Surface::OpenAi,
        TransportMode::Unary,
        seed.as_bytes(),
    )
    .unwrap()
    .into_iter()
    .map(|attempt| {
        route
            .targets
            .iter()
            .find(|target| target.id == attempt.target_id)
            .unwrap()
            .routing_id
            .unwrap()
            .as_uuid()
    })
    .collect::<Vec<_>>();
    assert_eq!(simulated_routing_ids, runtime_routing_ids);
}

async fn insert_unbased_route_draft(
    pool: &PgPool,
    actor: Uuid,
    slug: &str,
    model_ids: &[Uuid],
) -> DraftFixture {
    let fixture = DraftFixture {
        id: Uuid::now_v7(),
        etag: Uuid::now_v7(),
    };
    sqlx::query(
        "INSERT INTO route_drafts \
         (id, routing_id, slug, state, overall_timeout_ms, max_attempts, etag, created_by) \
         VALUES ($1, $2, $3, 'draft', 30000, $4, $5, $6)",
    )
    .bind(fixture.id)
    .bind(Uuid::now_v7())
    .bind(slug)
    .bind(i16::try_from(model_ids.len()).unwrap())
    .bind(fixture.etag)
    .bind(actor)
    .execute(pool)
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO route_draft_operations (route_draft_id, operation) VALUES ($1, 'video_get')",
    )
    .bind(fixture.id)
    .execute(pool)
    .await
    .unwrap();
    for (position, model_id) in model_ids.iter().enumerate() {
        sqlx::query(
            "INSERT INTO route_draft_targets \
             (id, routing_id, route_draft_id, provider_model_id, priority, weight, timeout_ms, position) \
             VALUES ($1, $2, $3, $4, 0, 1, 20000, $5)",
        )
        .bind(Uuid::now_v7())
        .bind(Uuid::now_v7())
        .bind(fixture.id)
        .bind(model_id)
        .bind(i32::try_from(position).unwrap())
        .execute(pool)
        .await
        .unwrap();
    }
    fixture
}

async fn revision_target_identities(pool: &PgPool, revision_id: Uuid) -> Vec<(Uuid, Uuid)> {
    sqlx::query(
        "SELECT id, routing_id FROM route_revision_targets \
         WHERE route_revision_id = $1 ORDER BY position",
    )
    .bind(revision_id)
    .fetch_all(pool)
    .await
    .unwrap()
    .into_iter()
    .map(|row| (row.get("id"), row.get("routing_id")))
    .collect()
}
