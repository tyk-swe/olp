use std::num::NonZeroU32;

use chrono::{Duration, Utc};
use olp_domain::{ApiKeyScope, RuntimeSnapshot};
use olp_storage::{NewOwner, PgStore, SessionMaterial};
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn fallback_uses_current_keys_and_release_exact_provider_transport_config() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 5).await.unwrap();
    store.migrate().await.unwrap();
    let (owner, _) = store
        .setup_owner_with_session(
            NewOwner {
                installation_name: "Fallback integrity".to_owned(),
                email: "owner@fallback.test".to_owned(),
                display_name: "Owner".to_owned(),
                password_hash: "test-password-hash".to_owned(),
            },
            &SessionMaterial::generate(),
            Duration::hours(1),
        )
        .await
        .unwrap();
    let actor = owner.user_id;

    let provider_id = Uuid::now_v7();
    let credential_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers \
         (id, name, kind, state, endpoint, auth_mode, etag, created_by) \
         VALUES ($1, 'fallback-provider', 'openai', 'active'::provider_state, \
                 'https://old.example.test/v1/', 'api_key', $2, $3)",
    )
    .bind(provider_id)
    .bind(Uuid::now_v7())
    .bind(actor)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO provider_credential_versions \
         (id, provider_id, version, ciphertext, nonce, master_key_version, created_by) \
         VALUES ($1, $2, 1, $3, $4, 1, $5)",
    )
    .bind(credential_id)
    .bind(provider_id)
    .bind(vec![7_u8; 32])
    .bind(vec![8_u8; 12])
    .bind(actor)
    .execute(store.pool())
    .await
    .unwrap();
    let provider_revision_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO provider_revisions \
         (id, provider_id, revision, name, kind, endpoint, auth_mode, connector_ready, \
          credential_version_id, source_etag, activated_by) \
         SELECT $1, id, 1, name, kind, endpoint, auth_mode, connector_ready, $2, etag, $3 \
         FROM providers WHERE id = $4",
    )
    .bind(provider_revision_id)
    .bind(credential_id)
    .bind(actor)
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "UPDATE providers SET active_credential_version_id = $1, active_revision_id = $2 \
         WHERE id = $3",
    )
    .bind(credential_id)
    .bind(provider_revision_id)
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();

    let key_id = Uuid::now_v7();
    let expires_at = Utc::now() + Duration::days(30);
    sqlx::query(
        "INSERT INTO api_keys \
         (id, lookup_id, secret_digest, name, created_by, expires_at, etag) \
         VALUES ($1, 'lookup_same_key', $2, 'fallback key', $3, $4, $5)",
    )
    .bind(key_id)
    .bind(vec![1_u8; 32])
    .bind(actor)
    .bind(expires_at)
    .bind(Uuid::now_v7())
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query("INSERT INTO api_key_scopes (api_key_id, scope) VALUES ($1, 'inference')")
        .bind(key_id)
        .execute(store.pool())
        .await
        .unwrap();

    let release = store.compile_and_publish_runtime(actor).await.unwrap();
    let historical: RuntimeSnapshot = serde_json::from_slice(&release.payload).unwrap();
    let historical_key = historical.api_keys.values().next().unwrap();
    assert_eq!(historical_key.digest.as_bytes(), &[1; 32]);
    assert_eq!(historical_key.scopes, [ApiKeyScope::Inference].into());
    assert!(historical_key.allowed_routes.is_empty());
    assert!(historical_key.limits.requests_per_minute.is_none());
    assert_eq!(
        store
            .provider_secrets_for_runtime(&historical)
            .await
            .unwrap()
            .len(),
        1
    );

    // Transport-affecting settings are release-exact sidecar authority. A
    // mutable replacement draft must not alter the historical connector.
    sqlx::query(
        "UPDATE providers SET endpoint = 'https://new.example.test/v1/', updated_at = now() \
         WHERE id = $1",
    )
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
    let historical_secret = store
        .provider_secrets_for_runtime(&historical)
        .await
        .unwrap();
    assert_eq!(
        historical_secret[0].endpoint.as_deref(),
        Some("https://old.example.test/v1/")
    );
    sqlx::query(
        "UPDATE providers SET endpoint = 'https://old.example.test/v1/', updated_at = now() \
         WHERE id = $1",
    )
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
    assert_eq!(
        store
            .provider_secrets_for_runtime(&historical)
            .await
            .unwrap()
            .len(),
        1,
        "an exact transport-config match remains valid after metadata timestamps change"
    );

    // Keep the lookup ID stable while changing every security-relevant field.
    // This is the case a lookup-only fallback filter cannot secure.
    let current_expiry = Utc::now() + Duration::minutes(20);
    sqlx::query(
        "UPDATE api_keys SET secret_digest = $1, requests_per_minute = 11, \
                max_concurrency = 3, expires_at = $2 WHERE id = $3",
    )
    .bind(vec![2_u8; 32])
    .bind(current_expiry)
    .bind(key_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query("DELETE FROM api_key_scopes WHERE api_key_id = $1")
        .bind(key_id)
        .execute(store.pool())
        .await
        .unwrap();
    sqlx::query("INSERT INTO api_key_scopes (api_key_id, scope) VALUES ($1, 'models_read')")
        .bind(key_id)
        .execute(store.pool())
        .await
        .unwrap();
    sqlx::query(
        "INSERT INTO api_key_route_allowlist (api_key_id, route_slug) \
         VALUES ($1, 'restricted')",
    )
    .bind(key_id)
    .execute(store.pool())
    .await
    .unwrap();

    let current = store.current_runtime_api_keys().await.unwrap();
    let current_key = current.values().next().unwrap();
    assert_eq!(current_key.lookup_id.as_str(), "lookup_same_key");
    assert_eq!(current_key.digest.as_bytes(), &[2; 32]);
    assert_eq!(
        current_key.expires_at.map(|value| value.timestamp_micros()),
        Some(current_expiry.timestamp_micros())
    );
    assert_eq!(current_key.scopes, [ApiKeyScope::ModelsRead].into());
    assert_eq!(
        current_key.allowed_routes.iter().next().unwrap().as_str(),
        "restricted"
    );
    assert_eq!(
        current_key.limits.requests_per_minute.map(NonZeroU32::get),
        Some(11)
    );
    assert_eq!(current_key.limits.concurrency.map(NonZeroU32::get), Some(3));
}
