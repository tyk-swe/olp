use chrono::{Duration, Utc};
use olp_domain::{ApiKeyLimits, ApiKeyScope, Role};
use olp_storage::{
    AccessError, AuthHmacKey, ConfigurationError, IdempotencyOutcome, IdempotencyResponse,
    IdentityError, InstallationSetupInput, MasterKey, NewApiKeyRecord, NewInvitation, PgStore,
    ReplayableIdempotency, RotateApiKeyInput, hash_password, idempotency_fingerprint,
};
use serde_json::{Value, json};
use sqlx::Executor as _;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn encrypted_idempotency_replays_one_time_secrets_after_a_lost_response() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 8).await.unwrap();
    store.migrate().await.unwrap();
    let owner = store
        .setup_installation(InstallationSetupInput {
            installation_name: "Idempotency replay integration".to_owned(),
            email: "owner@idempotency.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash_password("correct horse battery staple").unwrap(),
        })
        .await
        .unwrap();
    let actor = owner.user_id;
    let master_key = MasterKey::new(7, [42; 32]);

    let invitation_key = "invite-replay-test-001";
    let invitation_request = json!({
        "email": "lost-response@idempotency.test",
        "role": "developer",
        "expires_in_hours": 24
    });
    let invitation_fingerprint = idempotency_fingerprint(&invitation_request).unwrap();
    let invitation_expires_at = Utc::now() + Duration::hours(24);
    let first_invitation = store
        .create_invitation(
            NewInvitation {
                email: "lost-response@idempotency.test".to_owned(),
                role: Role::Developer,
                expires_at: invitation_expires_at,
                actor,
                idempotency_key: invitation_key.to_owned(),
            },
            ReplayableIdempotency::new(invitation_fingerprint, &master_key),
            |created| {
                IdempotencyResponse::json(
                    201,
                    &json!({
                        "invitation": {
                            "id": created.invitation.id,
                            "email": created.invitation.email,
                            "role": created.invitation.role.as_str()
                        },
                        "token": created.material.token()
                    }),
                    None,
                )
            },
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed {
        response: first_invitation_response,
        ..
    } = first_invitation
    else {
        panic!("the first invitation request must execute");
    };
    let (status, _, _, first_invitation_body) = first_invitation_response.into_parts();
    assert_eq!(status, 201);
    let first_invitation_json: Value = serde_json::from_slice(&first_invitation_body).unwrap();
    let invitation_token = first_invitation_json["token"].as_str().unwrap().to_owned();

    // The caller can lose the response after the transaction commits. An
    // identical retry must recover the exact encrypted status and body, and
    // must never invoke a second response/secret builder.
    let replayed_invitation = store
        .create_invitation(
            NewInvitation {
                email: "lost-response@idempotency.test".to_owned(),
                role: Role::Developer,
                expires_at: invitation_expires_at,
                actor,
                idempotency_key: invitation_key.to_owned(),
            },
            ReplayableIdempotency::new(invitation_fingerprint, &master_key),
            |_| panic!("completed invitation replay must not build a new secret"),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Replayed(replayed_invitation_response) = replayed_invitation else {
        panic!("the duplicate invitation request must replay");
    };
    let (status, _, _, replayed_invitation_body) = replayed_invitation_response.into_parts();
    assert_eq!(status, 201);
    assert_eq!(replayed_invitation_body, first_invitation_body);

    let replay_row = sqlx::query(
        "SELECT resource_id, replay_ciphertext, replay_nonce, replay_key_version \
         FROM idempotency_records \
         WHERE actor_user_id = $1 AND operation = 'invitation.create' AND idempotency_key = $2",
    )
    .bind(actor)
    .bind(invitation_key)
    .fetch_one(store.pool())
    .await
    .unwrap();
    let resource_id: Option<String> = sqlx::Row::get(&replay_row, "resource_id");
    let ciphertext: Vec<u8> = sqlx::Row::get(&replay_row, "replay_ciphertext");
    let nonce: Vec<u8> = sqlx::Row::get(&replay_row, "replay_nonce");
    let key_version: i32 = sqlx::Row::get(&replay_row, "replay_key_version");
    assert!(resource_id.is_none());
    assert_eq!(nonce.len(), 12);
    assert_eq!(key_version, 7);
    assert!(
        !ciphertext
            .windows(invitation_token.len())
            .any(|window| window == invitation_token.as_bytes())
    );

    let changed_fingerprint = idempotency_fingerprint(&json!({
        "email": "changed@idempotency.test",
        "role": "viewer",
        "expires_in_hours": 24
    }))
    .unwrap();
    assert!(matches!(
        store
            .create_invitation(
                NewInvitation {
                    email: "changed@idempotency.test".to_owned(),
                    role: Role::Viewer,
                    expires_at: invitation_expires_at,
                    actor,
                    idempotency_key: invitation_key.to_owned(),
                },
                ReplayableIdempotency::new(changed_fingerprint, &master_key),
                |_| panic!("a mismatched request must not execute"),
            )
            .await,
        Err(IdentityError::IdempotencyConflict)
    ));

    // A second transaction carrying the same key observes the nonblocking
    // advisory lock and fails as in-progress instead of waiting and executing.
    let concurrent_key = "invite-concurrent-test-001";
    let concurrent_scope = format!("olp:v2:idempotency:{actor}:invitation.create:{concurrent_key}");
    let mut concurrent_transaction = store.pool().begin().await.unwrap();
    concurrent_transaction
        .execute(
            sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))")
                .bind(&concurrent_scope),
        )
        .await
        .unwrap();
    let concurrent_fingerprint = idempotency_fingerprint(&json!({
        "email": "concurrent@idempotency.test",
        "role": "viewer",
        "expires_in_hours": 24
    }))
    .unwrap();
    assert!(matches!(
        store
            .create_invitation(
                NewInvitation {
                    email: "concurrent@idempotency.test".to_owned(),
                    role: Role::Viewer,
                    expires_at: invitation_expires_at,
                    actor,
                    idempotency_key: concurrent_key.to_owned(),
                },
                ReplayableIdempotency::new(concurrent_fingerprint, &master_key),
                |_| panic!("a concurrent duplicate must not execute"),
            )
            .await,
        Err(IdentityError::IdempotencyInProgress)
    ));
    concurrent_transaction.rollback().await.unwrap();

    let auth_hmac_key = AuthHmacKey::new([9; 32]);
    let create_key = "api-key-replay-test-001";
    let create_fingerprint = idempotency_fingerprint(&json!({
        "name": "Replay key",
        "scopes": ["inference"],
        "allowed_routes": []
    }))
    .unwrap();
    let first_material = auth_hmac_key.generate_api_key();
    let first_secret = first_material.expose_once().to_owned();
    let first_key = store
        .create_api_key_record(
            &NewApiKeyRecord {
                name: "Replay key".to_owned(),
                material: first_material,
                scopes: vec![ApiKeyScope::Inference],
                allowed_routes: vec![],
                limits: ApiKeyLimits::default(),
                expires_at: None,
                actor,
                idempotency_key: create_key.to_owned(),
            },
            ReplayableIdempotency::new(create_fingerprint, &master_key),
            |created| {
                IdempotencyResponse::json(
                    201,
                    &json!({
                        "id": created.id,
                        "lookup_id": created.lookup_id,
                        "secret": first_secret,
                        "runtime_generation": {
                            "id": created.release.generation_id,
                            "sequence": created.release.sequence
                        }
                    }),
                    Some(format!("\"{}\"", created.etag)),
                )
            },
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed {
        value: created_key,
        response: first_key_response,
    } = first_key
    else {
        panic!("the first API-key request must execute");
    };
    let first_key_parts = first_key_response.into_parts();
    let first_key_json: Value = serde_json::from_slice(&first_key_parts.3).unwrap();
    assert!(
        first_key_json["secret"]
            .as_str()
            .unwrap()
            .starts_with("olp_v2_")
    );

    let replay_material = auth_hmac_key.generate_api_key();
    let replayed_key = store
        .create_api_key_record(
            &NewApiKeyRecord {
                name: "Replay key".to_owned(),
                material: replay_material,
                scopes: vec![ApiKeyScope::Inference],
                allowed_routes: vec![],
                limits: ApiKeyLimits::default(),
                expires_at: None,
                actor,
                idempotency_key: create_key.to_owned(),
            },
            ReplayableIdempotency::new(create_fingerprint, &master_key),
            |_| panic!("completed API-key replay must not build a new secret"),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Replayed(replayed_key_response) = replayed_key else {
        panic!("the duplicate API-key request must replay");
    };
    assert_eq!(replayed_key_response.into_parts(), first_key_parts);

    let rotation_key = "api-key-rotation-replay-test-001";
    let rotation_fingerprint = idempotency_fingerprint(&json!({
        "api_key_id": created_key.id,
        "expected_etag": created_key.etag
    }))
    .unwrap();
    let rotation_material = auth_hmac_key.generate_api_key();
    let rotation_secret = rotation_material.expose_once().to_owned();
    let first_rotation = store
        .rotate_api_key(
            RotateApiKeyInput {
                id: created_key.id,
                material: &rotation_material,
                expected_etag: created_key.etag,
                actor,
                idempotency_key: rotation_key,
            },
            ReplayableIdempotency::new(rotation_fingerprint, &master_key),
            |rotation| {
                IdempotencyResponse::json(
                    200,
                    &json!({
                        "id": rotation.id,
                        "lookup_id": rotation.lookup_id,
                        "secret": rotation_secret,
                        "etag": rotation.etag,
                        "runtime_generation": {
                            "id": rotation.release.generation_id,
                            "sequence": rotation.release.sequence
                        }
                    }),
                    Some(format!("\"{}\"", rotation.etag)),
                )
            },
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed {
        response: first_rotation_response,
        ..
    } = first_rotation
    else {
        panic!("the first API-key rotation must execute");
    };
    let first_rotation_parts = first_rotation_response.into_parts();

    let discarded_rotation_material = auth_hmac_key.generate_api_key();
    let replayed_rotation = store
        .rotate_api_key(
            RotateApiKeyInput {
                id: created_key.id,
                material: &discarded_rotation_material,
                expected_etag: created_key.etag,
                actor,
                idempotency_key: rotation_key,
            },
            ReplayableIdempotency::new(rotation_fingerprint, &master_key),
            |_| panic!("completed rotation replay must not build a new secret"),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Replayed(replayed_rotation_response) = replayed_rotation else {
        panic!("the duplicate API-key rotation must replay");
    };
    assert_eq!(
        replayed_rotation_response.into_parts(),
        first_rotation_parts
    );

    let changed_create_fingerprint = idempotency_fingerprint(&json!({
        "name": "Changed replay key",
        "scopes": ["inference"],
        "allowed_routes": []
    }))
    .unwrap();
    let changed_material = auth_hmac_key.generate_api_key();
    assert!(matches!(
        store
            .create_api_key_record(
                &NewApiKeyRecord {
                    name: "Changed replay key".to_owned(),
                    material: changed_material,
                    scopes: vec![ApiKeyScope::Inference],
                    allowed_routes: vec![],
                    limits: ApiKeyLimits::default(),
                    expires_at: None,
                    actor,
                    idempotency_key: create_key.to_owned(),
                },
                ReplayableIdempotency::new(changed_create_fingerprint, &master_key),
                |_| panic!("a mismatched API-key request must not execute"),
            )
            .await,
        Err(AccessError::IdempotencyConflict)
    ));

    let changed_rotation_fingerprint = idempotency_fingerprint(&json!({
        "api_key_id": created_key.id,
        "expected_etag": "different"
    }))
    .unwrap();
    let changed_rotation_material = auth_hmac_key.generate_api_key();
    assert!(matches!(
        store
            .rotate_api_key(
                RotateApiKeyInput {
                    id: created_key.id,
                    material: &changed_rotation_material,
                    expected_etag: created_key.etag,
                    actor,
                    idempotency_key: rotation_key,
                },
                ReplayableIdempotency::new(changed_rotation_fingerprint, &master_key),
                |_| panic!("a mismatched rotation must not execute"),
            )
            .await,
        Err(ConfigurationError::IdempotencyConflict)
    ));
}
