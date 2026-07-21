use std::time::Duration;

use olp_domain::{ApiKeyLimits, ApiKeyScope, RuntimeSnapshot};
use olp_storage::{
    AuthHmacKey, IdempotencyOutcome, IdempotencyResponse, InstallationSetupInput, MasterKey,
    NewApiKeyRecord,
    PgStore, ReplayableIdempotency, hash_password, idempotency_fingerprint,
};
use uuid::Uuid;

const PUBLICATION_LOCK_ID: i64 = 0x4f4c_505f_5254;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn replayable_key_creation_takes_its_snapshot_after_the_publication_lock() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 5).await.unwrap();
    store.migrate().await.unwrap();
    let owner = store
        .setup_installation(InstallationSetupInput {
            installation_name: "Runtime publication integration".to_owned(),
            email: "owner@example.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash_password("correct horse battery staple").unwrap(),
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
        let fingerprint = idempotency_fingerprint(&"runtime-lock-order-0001").unwrap();
        creating_store
            .create_api_key_record(
                &key,
                ReplayableIdempotency::new(fingerprint, &master_key),
                |_| IdempotencyResponse::new(201, None, None, Vec::new()),
            )
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

    let IdempotencyOutcome::Executed { value, .. } = creation.await.unwrap() else {
        panic!("fresh key creation unexpectedly replayed");
    };
    let snapshot: RuntimeSnapshot = serde_json::from_slice(&value.release.payload).unwrap();
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
