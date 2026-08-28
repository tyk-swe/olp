use std::time::Duration;

use olp_db::{
    access::NewApiKeyRecord, idempotency::Outcome, idempotency::Replayable, idempotency::Response,
    idempotency::fingerprint, identity::InstallationSetupInput, security::envelope::MasterKey,
    security::key_material::AuthHmacKey, security::password::hash,
};
use olp_engine::domain::{
    auth::{ApiKeyLimits, ApiKeyScope},
    ids::RouteSlug,
    routing::snapshot::Snapshot,
};
use uuid::Uuid;

use crate::support::route_fixtures::{insert_provider, insert_unbased_route_draft};

const PUBLICATION_LOCK_ID: i64 = 0x4f4c_505f_5254;

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn replayable_key_creation_takes_its_snapshot_after_the_publication_lock() {
    let db = olp_db::test_support::TestDb::create_migrated("runtime_publication").await;
    let store = db.store(5).await;
    let owner = store
        .setup_installation(InstallationSetupInput {
            installation_name: "Runtime publication integration".to_owned(),
            email: "owner@example.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash("correct horse battery staple").unwrap(),
        })
        .await
        .unwrap();

    // Hold the same lock as a winning runtime mutation. The key-creation task
    // must wait before inspecting runtime authority, then include everything
    // committed by this transaction in the release it publishes next.
    let mut winner = store.pool().begin().await.unwrap();
    sqlx::query("SELECT pg_advisory_xact_lock($1)")
        .bind(PUBLICATION_LOCK_ID)
        .execute(&mut *winner)
        .await
        .unwrap();

    let creating_store = store.clone();
    let actor = owner.user_id;
    let creation = tokio::spawn(async move {
        let auth_hmac_key = AuthHmacKey::new([31; 32]);
        let master_key = MasterKey::new(1, [37; 32]);
        let key = NewApiKeyRecord {
            name: "waiting key".to_owned(),
            material: auth_hmac_key.generate_api_key(),
            scopes: vec![ApiKeyScope::Inference],
            allowed_routes: Vec::new(),
            limits: ApiKeyLimits::default(),
            expires_at: None,
            actor,
            idempotency_key: "runtime-lock-order-0001".to_owned(),
        };
        let fingerprint = fingerprint(&"runtime-lock-order-0001").unwrap();
        creating_store
            .create_api_key_record(&key, Replayable::new(fingerprint, &master_key), |_| {
                Response::new(201, None, None, Vec::new())
            })
            .await
            .unwrap()
    });

    let mut waiting = false;
    for _ in 0..100 {
        waiting = sqlx::query_scalar(
            "SELECT EXISTS (SELECT 1 FROM pg_locks \
             WHERE locktype = 'advisory' AND NOT granted)",
        )
        .fetch_one(store.pool())
        .await
        .unwrap();
        if waiting {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    assert!(
        waiting,
        "key creation did not wait for the publication lock"
    );

    let winner_auth_hmac_key = AuthHmacKey::new([41; 32]);
    let winner_material = winner_auth_hmac_key.generate_api_key();
    let winner_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO api_keys \
         (id, lookup_id, secret_digest, name, created_by, etag) \
         VALUES ($1, $2, $3, 'winning key', $4, $5)",
    )
    .bind(winner_id)
    .bind(&winner_material.lookup_id)
    .bind(winner_material.digest.as_slice())
    .bind(owner.user_id)
    .bind(Uuid::now_v7())
    .execute(&mut *winner)
    .await
    .unwrap();
    sqlx::query("INSERT INTO api_key_scopes (api_key_id, scope) VALUES ($1, 'inference')")
        .bind(winner_id)
        .execute(&mut *winner)
        .await
        .unwrap();
    winner.commit().await.unwrap();

    let Outcome::Executed { value, .. } = creation.await.unwrap() else {
        panic!("fresh key creation unexpectedly replayed");
    };
    let snapshot: Snapshot = serde_json::from_slice(&value.release.payload).unwrap();
    assert!(
        snapshot
            .api_keys
            .keys()
            .any(|lookup| lookup.as_str() == winner_material.lookup_id)
    );
    assert!(
        snapshot
            .api_keys
            .keys()
            .any(|lookup| lookup.as_str() == value.lookup_id)
    );

    // A rolling old binary pins a REPEATABLE READ snapshot before waiting on
    // the publication lock. The database guard must reject its generation so
    // it cannot publish stale route/provider state after this new writer.
    let generation_count: i64 = sqlx::query_scalar("SELECT count(*) FROM runtime_generations")
        .fetch_one(store.pool())
        .await
        .unwrap();
    let mut legacy = store
        .pool()
        .begin_with("BEGIN ISOLATION LEVEL REPEATABLE READ")
        .await
        .unwrap();
    let error = sqlx::query(
        "INSERT INTO runtime_generations \
         (id, compiled_release, release_sha256, created_by) VALUES ($1, $2, $3, $4)",
    )
    .bind(Uuid::now_v7())
    .bind([1_u8].as_slice())
    .bind([2_u8; 32].as_slice())
    .bind(owner.user_id)
    .execute(&mut *legacy)
    .await
    .unwrap_err();
    assert_eq!(
        error
            .as_database_error()
            .and_then(sqlx::error::DatabaseError::code)
            .as_deref(),
        Some("55000")
    );
    legacy.rollback().await.unwrap();
    let guarded_generation_count: i64 =
        sqlx::query_scalar("SELECT count(*) FROM runtime_generations")
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(guarded_generation_count, generation_count);
}

/// The compiler reads route children and API key children with one query per
/// table for the whole snapshot. Every route must still get exactly its own
/// targets in position order, and every key its own scopes and allowlist.
#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn batched_compilation_keeps_targets_ordered_and_key_sets_separate() {
    let db = olp_db::test_support::TestDb::create_migrated("runtime_batched_compile").await;
    let store = db.store(5).await;
    let owner = store
        .setup_installation(InstallationSetupInput {
            installation_name: "Batched compilation".to_owned(),
            email: "owner@batched-compile.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash("correct horse battery staple").unwrap(),
        })
        .await
        .unwrap();
    let actor = owner.user_id;
    let first = insert_provider(store.pool(), actor, "compile-first").await;
    let second = insert_provider(store.pool(), actor, "compile-second").await;
    for (slug, models) in [
        ("compile-wide", vec![second.model_id, first.model_id]),
        ("compile-narrow", vec![first.model_id]),
    ] {
        let draft = insert_unbased_route_draft(store.pool(), actor, slug, &models).await;
        let (etag, _) = store
            .validate_route_draft(draft.id, draft.etag, actor)
            .await
            .unwrap();
        store
            .activate_route_draft(draft.id, etag, actor, &format!("activate-{slug}"))
            .await
            .unwrap();
    }

    let auth_hmac_key = AuthHmacKey::new([31; 32]);
    let master_key = MasterKey::new(1, [37; 32]);
    for (name, scopes, allowed_routes) in [
        (
            "scoped",
            vec![ApiKeyScope::Inference, ApiKeyScope::ModelsRead],
            vec![RouteSlug::parse("compile-wide").unwrap()],
        ),
        ("open", vec![ApiKeyScope::Inference], Vec::new()),
    ] {
        let key = NewApiKeyRecord {
            name: name.to_owned(),
            material: auth_hmac_key.generate_api_key(),
            scopes,
            allowed_routes,
            limits: ApiKeyLimits::default(),
            expires_at: None,
            actor,
            idempotency_key: format!("batched-compile-{name}"),
        };
        let fingerprint = fingerprint(&key.idempotency_key).unwrap();
        store
            .create_api_key_record(&key, Replayable::new(fingerprint, &master_key), |_| {
                Response::new(201, None, None, Vec::new())
            })
            .await
            .unwrap();
    }

    let release = store.compile_and_publish_runtime(actor).await.unwrap();
    let runtime: Snapshot = serde_json::from_slice(&release.payload).unwrap();
    let targets = |slug: &str| {
        runtime.routes[&RouteSlug::parse(slug).unwrap()]
            .targets
            .iter()
            .map(|target| target.upstream_model.as_str())
            .collect::<Vec<_>>()
    };
    assert_eq!(
        targets("compile-wide"),
        vec!["compile-second-model", "compile-first-model"]
    );
    assert_eq!(targets("compile-narrow"), vec!["compile-first-model"]);
    assert!(
        runtime
            .routes
            .values()
            .all(|route| route.operations.len() == 1)
    );

    assert_eq!(runtime.api_keys.len(), 2);
    let scoped = runtime
        .api_keys
        .values()
        .find(|key| key.scopes.len() == 2)
        .expect("the scoped key compiles with both scopes");
    assert_eq!(
        scoped.allowed_routes.iter().collect::<Vec<_>>(),
        vec![&RouteSlug::parse("compile-wide").unwrap()]
    );
    let open = runtime
        .api_keys
        .values()
        .find(|key| key.scopes.len() == 1)
        .expect("the open key compiles with one scope");
    assert!(open.allowed_routes.is_empty());
}
