use chrono::Duration;
use chrono::Utc;
use olp::access::authentication::RecentAuthPurpose;
use olp::access::authentication::SessionSecurityContext;
use olp::access::identity::InstallationSetupInput;
use olp::access::oidc::repository::types::CompleteOidcLogin;
use olp::access::oidc::repository::types::NewOidcFlow;
use olp::access::oidc::repository::types::OidcError;
use olp::access::oidc::repository::types::OidcFlowPurpose;
use olp::access::oidc::repository::types::UpsertOidcConfiguration;
use olp::access::policy::Role;
use olp::crypto::aad::oidc_client_secret;
use olp::crypto::aad::oidc_flow_payload;
use olp::crypto::envelope::MasterKey;
use olp::crypto::password::hash;
use olp::crypto::session_material::RecentAuthMaterial;
use olp::crypto::session_material::SessionMaterial;
use sha2::Digest;
use sha2::Sha256;
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn oidc_flow_creation_is_bound_to_the_exact_enabled_configuration() {
    let db = olp::test_support::TestDb::create_migrated("oidc_flow").await;
    let pool = db.pool(5).await;
    let owner_session = SessionMaterial::generate();
    let (owner, owner_session_id) = olp::access::identity::setup::setup_installation_with_session(
        &pool,
        &olp::database::RequestProvenance::default(),
        InstallationSetupInput {
            installation_name: "OIDC flow integration".to_owned(),
            email: "owner@example.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash("correct horse battery staple").unwrap(),
        },
        &owner_session,
        Duration::hours(1),
    )
    .await
    .unwrap();
    let owner_principal =
        olp::access::authentication::sessions::session_principal(&pool, owner_session.token())
            .await
            .unwrap()
            .unwrap();
    assert_eq!(owner_principal.session_id, owner_session_id);
    let key = MasterKey::new(1, [53; 32]);
    let configuration_id = Uuid::now_v7();

    let first = olp::access::oidc::repository::configuration::upsert_oidc_configuration(
        &pool,
        &olp::database::RequestProvenance::default(),
        configuration(&key, configuration_id, owner.user_id, None, true),
    )
    .await
    .unwrap();
    let current = olp::access::oidc::repository::configuration::upsert_oidc_configuration(
        &pool,
        &olp::database::RequestProvenance::default(),
        configuration(
            &key,
            configuration_id,
            owner.user_id,
            Some(first.etag),
            true,
        ),
    )
    .await
    .unwrap();
    assert_eq!(
        current.updated_by_email.as_deref(),
        Some("owner@example.test")
    );

    // N-1 flow inserts omit the configuration ETag and must fail closed once
    // the rollout fence is present.
    let legacy_flow_error = sqlx::query(
        "INSERT INTO oidc_authorization_flows \
         (id, configuration_id, purpose, state_digest, browser_binding_digest, \
          encrypted_payload, payload_nonce, payload_key_version, expires_at) \
         VALUES ($1, $2, 'login', $3, $4, $5, $6, 1, now() + interval '5 minutes')",
    )
    .bind(Uuid::now_v7())
    .bind(configuration_id)
    .bind([1_u8; 32].as_slice())
    .bind([2_u8; 32].as_slice())
    .bind([4_u8; 16].as_slice())
    .bind([5_u8; 12].as_slice())
    .execute(&pool)
    .await
    .unwrap_err();
    assert_eq!(
        legacy_flow_error
            .as_database_error()
            .and_then(sqlx::error::DatabaseError::code)
            .as_deref(),
        Some("23502")
    );

    // Every OIDC completion touches its identity. Old callbacks do not set the
    // transaction-local configuration fence and are rejected atomically.
    let legacy_completion_error =
        sqlx::query("INSERT INTO oidc_identities (issuer, subject, user_id) VALUES ($1, $2, $3)")
            .bind("https://idp.example.test")
            .bind("legacy-callback")
            .bind(owner.user_id)
            .execute(&pool)
            .await
            .unwrap_err();
    assert_eq!(
        legacy_completion_error
            .as_database_error()
            .and_then(sqlx::error::DatabaseError::code)
            .as_deref(),
        Some("55000")
    );

    let link_grant = RecentAuthMaterial::generate();
    assert!(
        olp::access::authentication::issue_recent_authentication(
            &(pool),
            &olp::database::RequestProvenance::default(),
            SessionSecurityContext {
                session_id: owner_session_id,
                user_id: owner.user_id,
                security_version: owner_principal.security_version,
            },
            RecentAuthPurpose::OidcLink,
            None,
            &link_grant,
            Duration::minutes(5),
        )
        .await
        .unwrap()
    );

    let rejected_login = login_flow(&key, configuration_id, current.etag);
    assert!(matches!(
        olp::access::oidc::repository::flows::create_oidc_flow(
            &(pool),
            &olp::database::RequestProvenance::default(),
            rejected_login
        )
        .await,
        Err(OidcError::Invalid(_))
    ));

    let stale_flow = link_flow(
        &key,
        configuration_id,
        first.etag,
        owner.user_id,
        owner_session_id,
        owner_principal.security_version,
        link_grant.token_digest(),
    );
    assert!(matches!(
        olp::access::oidc::repository::flows::create_oidc_flow(
            &(pool),
            &olp::database::RequestProvenance::default(),
            stale_flow
        )
        .await,
        Err(OidcError::PreconditionFailed)
    ));
    let state = "s".repeat(43);
    let browser_binding = "b".repeat(43);
    let mut consumable = link_flow(
        &key,
        configuration_id,
        current.etag,
        owner.user_id,
        owner_session_id,
        owner_principal.security_version,
        link_grant.token_digest(),
    );
    consumable.state_digest = Sha256::digest(state.as_bytes()).into();
    consumable.browser_binding_digest = Sha256::digest(browser_binding.as_bytes()).into();
    let flow_id = consumable.id;
    olp::access::oidc::repository::flows::create_oidc_flow(
        &pool,
        &olp::database::RequestProvenance::default(),
        consumable,
    )
    .await
    .unwrap();
    assert!(matches!(
        olp::access::oidc::repository::flows::consume_oidc_flow(
            &(pool),
            Uuid::now_v7(),
            &state,
            &browser_binding,
            owner_session_id
        )
        .await,
        Err(OidcError::FlowUnavailable)
    ));
    assert!(matches!(
        olp::access::oidc::repository::flows::consume_oidc_flow(
            &(pool),
            flow_id,
            &state,
            &browser_binding,
            Uuid::now_v7()
        )
        .await,
        Err(OidcError::FlowSessionMismatch)
    ));
    let consumed = olp::access::oidc::repository::flows::consume_oidc_flow(
        &pool,
        flow_id,
        &state,
        &browser_binding,
        owner_session_id,
    )
    .await
    .unwrap();
    assert_eq!(consumed.id, flow_id);
    assert_eq!(consumed.actor_session_id, Some(owner_session_id));
    assert_eq!(
        consumed.actor_security_version,
        Some(owner_principal.security_version)
    );
    assert!(matches!(
        olp::access::oidc::repository::flows::consume_oidc_flow(
            &(pool),
            flow_id,
            &state,
            &browser_binding,
            owner_session_id
        )
        .await,
        Err(OidcError::FlowUnavailable)
    ));

    // Configuration changes still invalidate any independently outstanding
    // link redirect.
    let second_link_grant = RecentAuthMaterial::generate();
    assert!(
        olp::access::authentication::issue_recent_authentication(
            &(pool),
            &olp::database::RequestProvenance::default(),
            SessionSecurityContext {
                session_id: owner_session_id,
                user_id: owner.user_id,
                security_version: owner_principal.security_version,
            },
            RecentAuthPurpose::OidcLink,
            None,
            &second_link_grant,
            Duration::minutes(5),
        )
        .await
        .unwrap()
    );
    olp::access::oidc::repository::flows::create_oidc_flow(
        &pool,
        &olp::database::RequestProvenance::default(),
        link_flow(
            &key,
            configuration_id,
            current.etag,
            owner.user_id,
            owner_session_id,
            owner_principal.security_version,
            second_link_grant.token_digest(),
        ),
    )
    .await
    .unwrap();
    let flow_count: i64 = sqlx::query_scalar("SELECT count(*) FROM oidc_authorization_flows")
        .fetch_one(&pool)
        .await
        .unwrap();
    assert_eq!(flow_count, 1);

    let disabled = olp::access::oidc::repository::configuration::upsert_oidc_configuration(
        &pool,
        &olp::database::RequestProvenance::default(),
        configuration(
            &key,
            configuration_id,
            owner.user_id,
            Some(current.etag),
            false,
        ),
    )
    .await
    .unwrap();
    assert!(matches!(
        olp::access::oidc::repository::flows::create_oidc_flow(
            &(pool),
            &olp::database::RequestProvenance::default(),
            link_flow(
                &key,
                configuration_id,
                disabled.etag,
                owner.user_id,
                owner_session_id,
                owner_principal.security_version,
                second_link_grant.token_digest(),
            )
        )
        .await,
        Err(OidcError::Disabled)
    ));
    let flow_count: i64 = sqlx::query_scalar("SELECT count(*) FROM oidc_authorization_flows")
        .fetch_one(&pool)
        .await
        .unwrap();
    assert_eq!(flow_count, 0);

    // Browser-held login material does not create a row at authorization
    // start, but its authenticated flow ID is globally claimable exactly once
    // when the callback arrives.
    let stateless_login_id = Uuid::now_v7();
    let stateless_login_expiry = Utc::now() + Duration::minutes(10);
    olp::access::oidc::repository::flows::consume_oidc_login_flow(
        &pool,
        stateless_login_id,
        stateless_login_expiry,
    )
    .await
    .unwrap();
    assert!(matches!(
        olp::access::oidc::repository::flows::consume_oidc_login_flow(
            &(pool),
            stateless_login_id,
            stateless_login_expiry
        )
        .await,
        Err(OidcError::FlowUnavailable)
    ));
    let consumption_count: i64 =
        sqlx::query_scalar("SELECT count(*) FROM oidc_login_flow_consumptions WHERE flow_id = $1")
            .bind(stateless_login_id)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(consumption_count, 1);

    let session = SessionMaterial::generate();
    assert!(matches!(
        olp::access::oidc::repository::identities::complete_oidc_login(
            &(pool),
            &olp::database::RequestProvenance::default(),
            CompleteOidcLogin {
                configuration_id,
                configuration_etag: current.etag,
                issuer: "https://idp.example.test",
                subject: "stale-subject",
                email: Some("stale@example.test"),
                display_name: Some("Stale User"),
                provisioning_role: Some(Role::Viewer),
                session: &session,
                session_ttl: Duration::hours(1),
            }
        )
        .await,
        Err(OidcError::PreconditionFailed)
    ));
    let stale_sessions: i64 =
        sqlx::query_scalar("SELECT count(*) FROM sessions WHERE token_digest = $1")
            .bind(session.token_digest().as_slice())
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(stale_sessions, 0);
}

fn configuration(
    key: &MasterKey,
    id: Uuid,
    actor: Uuid,
    expected_etag: Option<Uuid>,
    enabled: bool,
) -> UpsertOidcConfiguration {
    UpsertOidcConfiguration {
        id,
        discovery_url: "https://idp.example.test/.well-known/openid-configuration".to_owned(),
        issuer: "https://idp.example.test".to_owned(),
        authorization_endpoint: "https://idp.example.test/authorize".to_owned(),
        token_endpoint: "https://idp.example.test/token".to_owned(),
        jwks_uri: "https://idp.example.test/jwks".to_owned(),
        token_endpoint_auth_method: "client_secret_basic".to_owned(),
        client_id: "olp".to_owned(),
        encrypted_client_secret: key.seal(b"client-secret", &oidc_client_secret(id)).unwrap(),
        scopes: vec!["openid".to_owned(), "email".to_owned()],
        email_claim: "email".to_owned(),
        groups_claim: "groups".to_owned(),
        default_role: None,
        email_role_mappings: Vec::new(),
        group_role_mappings: Vec::new(),
        enabled,
        actor_user_id: actor,
        expected_etag,
    }
}

fn link_flow(
    key: &MasterKey,
    configuration_id: Uuid,
    configuration_etag: Uuid,
    actor_user_id: Uuid,
    actor_session_id: Uuid,
    actor_security_version: i64,
    recent_auth_token_digest: [u8; 32],
) -> NewOidcFlow {
    let id = Uuid::now_v7();
    NewOidcFlow {
        id,
        configuration_id,
        configuration_etag,
        purpose: OidcFlowPurpose::Link,
        actor_user_id: Some(actor_user_id),
        actor_session_id: Some(actor_session_id),
        actor_security_version: Some(actor_security_version),
        recent_auth_purpose: None,
        recent_auth_resource_id: None,
        recent_auth_token_digest: Some(recent_auth_token_digest),
        state_digest: Sha256::digest(Uuid::now_v7().as_bytes()).into(),
        browser_binding_digest: Sha256::digest(Uuid::now_v7().as_bytes()).into(),
        encrypted_payload: key.seal(b"flow-secret", &oidc_flow_payload(id)).unwrap(),
        expires_at: Utc::now() + Duration::minutes(5),
    }
}

fn login_flow(key: &MasterKey, configuration_id: Uuid, configuration_etag: Uuid) -> NewOidcFlow {
    let id = Uuid::now_v7();
    NewOidcFlow {
        id,
        configuration_id,
        configuration_etag,
        purpose: OidcFlowPurpose::Login,
        actor_user_id: None,
        actor_session_id: None,
        actor_security_version: None,
        recent_auth_purpose: None,
        recent_auth_resource_id: None,
        recent_auth_token_digest: None,
        state_digest: Sha256::digest(Uuid::now_v7().as_bytes()).into(),
        browser_binding_digest: Sha256::digest(Uuid::now_v7().as_bytes()).into(),
        encrypted_payload: key.seal(b"flow-secret", &oidc_flow_payload(id)).unwrap(),
        expires_at: Utc::now() + Duration::minutes(5),
    }
}
