use base64::{Engine as _, engine::general_purpose::STANDARD};
use olp_storage::{
    EncryptedSecret, MasterKey, NewOwner, PgStore, ReencryptionError, credential_aad,
    hash_password, idempotency_replay_aad, oidc_client_secret_aad, oidc_flow_payload_aad,
};
use sqlx::Row;
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn master_key_reencryption_is_authenticated_resumable_and_retirement_safe() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 5).await.unwrap();
    store.migrate().await.unwrap();
    let owner = store
        .setup_owner(NewOwner {
            installation_name: "Master-key integration".to_owned(),
            email: "owner@example.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash_password("correct horse battery staple").unwrap(),
        })
        .await
        .unwrap();

    let old_key = MasterKey::new(1, [17; 32]);
    let keyring = MasterKey::from_file_contents(&format!(
        r#"{{"active_version":2,"keys":[{{"version":1,"key":"{}"}},{{"version":2,"key":"{}"}}]}}"#,
        STANDARD.encode([17_u8; 32]),
        STANDARD.encode([29_u8; 32]),
    ))
    .unwrap();

    let provider_id = Uuid::now_v7();
    let credential_id = Uuid::now_v7();
    let credential_plaintext = b"provider-secret";
    let credential = old_key
        .seal(
            credential_plaintext,
            &credential_aad(provider_id, credential_id, 1),
        )
        .unwrap();
    sqlx::query(
        "INSERT INTO providers (id, name, kind, state, auth_mode, etag, created_by) \
         VALUES ($1, 'rotation-provider', 'openai', 'draft', 'api_key', $2, $3)",
    )
    .bind(provider_id)
    .bind(Uuid::now_v7())
    .bind(owner.user_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO provider_credential_versions \
         (id, provider_id, version, ciphertext, nonce, master_key_version, created_by) \
         VALUES ($1, $2, 1, $3, $4, $5, $6)",
    )
    .bind(credential_id)
    .bind(provider_id)
    .bind(&credential.ciphertext)
    .bind(credential.nonce.as_slice())
    .bind(i32::try_from(credential.key_version).unwrap())
    .bind(owner.user_id)
    .execute(store.pool())
    .await
    .unwrap();

    let configuration_id = Uuid::now_v7();
    let configuration_etag = Uuid::now_v7();
    let oidc_plaintext = b"oidc-client-secret";
    let oidc_secret = old_key
        .seal(oidc_plaintext, &oidc_client_secret_aad(configuration_id))
        .unwrap();
    sqlx::query(
        "INSERT INTO oidc_configurations \
         (id, issuer, client_id, encrypted_client_secret, secret_nonce, secret_key_version, \
          enabled, etag, updated_by) \
         VALUES ($1, 'https://issuer.example.test', 'olp', $2, $3, $4, false, $5, $6)",
    )
    .bind(configuration_id)
    .bind(&oidc_secret.ciphertext)
    .bind(oidc_secret.nonce.as_slice())
    .bind(i32::try_from(oidc_secret.key_version).unwrap())
    .bind(configuration_etag)
    .bind(owner.user_id)
    .execute(store.pool())
    .await
    .unwrap();

    let flow_id = Uuid::now_v7();
    let flow_plaintext = br#"{"nonce":"opaque","pkce":"opaque"}"#;
    let flow_secret = old_key
        .seal(flow_plaintext, &oidc_flow_payload_aad(flow_id))
        .unwrap();
    sqlx::query(
        "INSERT INTO oidc_authorization_flows \
         (id, configuration_id, configuration_etag, purpose, actor_user_id, state_digest, \
          browser_binding_digest, encrypted_payload, payload_nonce, payload_key_version, expires_at) \
         VALUES ($1, $2, $3, 'link', $4, $5, $6, $7, $8, $9, now() + interval '5 minutes')",
    )
    .bind(flow_id)
    .bind(configuration_id)
    .bind(configuration_etag)
    .bind(owner.user_id)
    .bind([31_u8; 32].as_slice())
    .bind([37_u8; 32].as_slice())
    .bind(&flow_secret.ciphertext)
    .bind(flow_secret.nonce.as_slice())
    .bind(i32::try_from(flow_secret.key_version).unwrap())
    .execute(store.pool())
    .await
    .unwrap();

    let idempotency_id = Uuid::now_v7();
    let operation = "master-key-test";
    let idempotency_key = "rotation-0001";
    let replay_plaintext = br#"{"status":201}"#;
    let replay_secret = old_key
        .seal(
            replay_plaintext,
            &idempotency_replay_aad(owner.user_id, operation, idempotency_key),
        )
        .unwrap();
    sqlx::query(
        "INSERT INTO idempotency_records \
         (id, actor_user_id, operation, idempotency_key, state, request_fingerprint, \
          replay_ciphertext, replay_nonce, replay_key_version, expires_at) \
         VALUES ($1, $2, $3, $4, 'completed', $5, $6, $7, $8, now() + interval '1 day')",
    )
    .bind(idempotency_id)
    .bind(owner.user_id)
    .bind(operation)
    .bind(idempotency_key)
    .bind([41_u8; 32].as_slice())
    .bind(&replay_secret.ciphertext)
    .bind(replay_secret.nonce.as_slice())
    .bind(i32::try_from(replay_secret.key_version).unwrap())
    .execute(store.pool())
    .await
    .unwrap();

    let initial = store
        .master_key_encryption_status(keyring.version())
        .await
        .unwrap();
    assert_eq!(initial.total_references(), 4);
    assert_eq!(initial.references_to(1), 4);
    assert_eq!(initial.non_active_references(), 4);
    assert!(matches!(
        store.verify_master_key_retirement(&keyring, 1, 2).await,
        Err(ReencryptionError::RetirementReferencesRemain {
            version: 1,
            references: 4
        })
    ));

    // Authentication happens before any update in a table transaction.
    let mut tampered = credential.ciphertext.clone();
    tampered[0] ^= 0x80;
    sqlx::query("UPDATE provider_credential_versions SET ciphertext = $1 WHERE id = $2")
        .bind(&tampered)
        .bind(credential_id)
        .execute(store.pool())
        .await
        .unwrap();
    assert!(matches!(
        store.reencrypt_master_key_batch(&keyring, 2).await,
        Err(ReencryptionError::Authentication { .. })
    ));
    let version_after_failure: i32 = sqlx::query_scalar(
        "SELECT master_key_version FROM provider_credential_versions WHERE id = $1",
    )
    .bind(credential_id)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(version_after_failure, 1);
    sqlx::query("UPDATE provider_credential_versions SET ciphertext = $1 WHERE id = $2")
        .bind(&credential.ciphertext)
        .bind(credential_id)
        .execute(store.pool())
        .await
        .unwrap();

    // One bounded batch models interruption. A later invocation resumes from
    // the envelope versions already committed and does not need a progress row.
    let first_batch = store.reencrypt_master_key_batch(&keyring, 2).await.unwrap();
    assert_eq!(first_batch.rows_reencrypted, 2);
    assert_eq!(
        store
            .master_key_encryption_status(keyring.version())
            .await
            .unwrap()
            .non_active_references(),
        2
    );
    let resumed_batch = store.reencrypt_master_key_batch(&keyring, 2).await.unwrap();
    assert_eq!(resumed_batch.rows_reencrypted, 2);
    assert_eq!(
        store
            .reencrypt_master_key_batch(&keyring, 2)
            .await
            .unwrap()
            .rows_reencrypted,
        0
    );

    let final_status = store
        .master_key_encryption_status(keyring.version())
        .await
        .unwrap();
    assert_eq!(final_status.total_references(), 4);
    assert_eq!(final_status.references_to(1), 0);
    assert_eq!(final_status.references_to(2), 4);
    assert_eq!(
        store
            .verify_master_key_retirement(&keyring, 1, 2)
            .await
            .unwrap()
            .rows_verified,
        4
    );

    assert_envelope_plaintext(
        &store,
        &keyring,
        "SELECT master_key_version AS key_version, nonce, ciphertext AS encrypted \
         FROM provider_credential_versions WHERE id = $1",
        credential_id,
        &credential_aad(provider_id, credential_id, 1),
        credential_plaintext,
    )
    .await;
    assert_envelope_plaintext(
        &store,
        &keyring,
        "SELECT secret_key_version AS key_version, secret_nonce AS nonce, \
                encrypted_client_secret AS encrypted FROM oidc_configurations WHERE id = $1",
        configuration_id,
        &oidc_client_secret_aad(configuration_id),
        oidc_plaintext,
    )
    .await;
    assert_envelope_plaintext(
        &store,
        &keyring,
        "SELECT payload_key_version AS key_version, payload_nonce AS nonce, \
                encrypted_payload AS encrypted FROM oidc_authorization_flows WHERE id = $1",
        flow_id,
        &oidc_flow_payload_aad(flow_id),
        flow_plaintext,
    )
    .await;
    assert_envelope_plaintext(
        &store,
        &keyring,
        "SELECT replay_key_version AS key_version, replay_nonce AS nonce, \
                replay_ciphertext AS encrypted FROM idempotency_records WHERE id = $1",
        idempotency_id,
        &idempotency_replay_aad(owner.user_id, operation, idempotency_key),
        replay_plaintext,
    )
    .await;
}

async fn assert_envelope_plaintext(
    store: &PgStore,
    master_key: &MasterKey,
    query: &'static str,
    row_id: Uuid,
    aad: &[u8],
    expected: &[u8],
) {
    let row = sqlx::query(query)
        .bind(row_id)
        .fetch_one(store.pool())
        .await
        .unwrap();
    let key_version = u32::try_from(row.get::<i32, _>("key_version")).unwrap();
    let nonce: Vec<u8> = row.get("nonce");
    let encrypted = EncryptedSecret {
        key_version,
        nonce: nonce.try_into().unwrap(),
        ciphertext: row.get("encrypted"),
    };
    assert_eq!(key_version, 2);
    assert_eq!(
        master_key.open(&encrypted, aad).unwrap().as_slice(),
        expected
    );
}
