use chrono::{DateTime, Duration, Utc};
use olp_db::{
    idempotency::{Outcome, Replayable, Response},
    identity::{AcceptInvitation, InstallationSetupInput, InvitationCreated, NewInvitation},
    oidc::types::{CompleteOidcLogin, UpsertOidcConfiguration},
    security::{
        aad::oidc_client_secret, envelope::MasterKey, password::hash,
        session_material::SessionMaterial,
    },
    store::Store,
};
use olp_engine::domain::auth::Role;
use uuid::Uuid;

async fn owner_id(store: &Store, label: &str) -> Uuid {
    let (owner, _) = store
        .setup_installation_with_session(
            InstallationSetupInput {
                installation_name: format!("Invitation Retirement {label}"),
                email: format!("owner@{label}.test"),
                display_name: "Owner".to_owned(),
                password_hash: hash("correct horse battery staple").unwrap(),
            },
            &SessionMaterial::generate(),
            Duration::hours(1),
        )
        .await
        .unwrap();
    owner.user_id
}

/// Mints one invitation through the idempotent writer. Every test here needs a
/// pending invitation to watch get retired; only the grantee and the minting
/// operator differ.
async fn mint_invitation(
    store: &Store,
    master_key: &MasterKey,
    seed: u8,
    email: &str,
    role: Role,
    actor: Uuid,
    idempotency_key: &str,
) -> InvitationCreated {
    let Outcome::Executed { value, .. } = store
        .create_invitation(
            NewInvitation {
                email: email.to_owned(),
                role,
                expires_at: Utc::now() + Duration::days(7),
                actor,
                idempotency_key: idempotency_key.to_owned(),
            },
            Replayable::new([seed; 32], master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap()
    else {
        panic!("{email} invitation must execute");
    };
    value
}

/// The revocation columns as the row itself carries them --- the store's own
/// read models resolve `revoked_by` to an email, which would hide a NULL.
async fn revocation(store: &Store, invitation_id: Uuid) -> (Option<DateTime<Utc>>, Option<Uuid>) {
    sqlx::query_as("SELECT revoked_at, revoked_by FROM invitations WHERE id = $1")
        .bind(invitation_id)
        .fetch_one(store.pool())
        .await
        .unwrap()
}

async fn audit_actor(store: &Store, action: &str, resource_id: Uuid) -> Option<Uuid> {
    sqlx::query_scalar(
        "SELECT actor_user_id FROM audit_events WHERE action = $1 AND resource_id = $2",
    )
    .bind(action)
    .bind(resource_id.to_string())
    .fetch_one(store.pool())
    .await
    .unwrap()
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn owner_demoted_through_update_user_role_retires_minted_invitations_attributed_to_admin() {
    let db = olp_db::test_support::TestDb::create_migrated("invitation-retirement").await;
    let store = db.store(5).await;
    let master_key = MasterKey::new(1, [17; 32]);
    let founder = owner_id(&store, "role-update-admin").await;

    let co_owner_invitation = mint_invitation(
        &store,
        &master_key,
        1,
        "co-owner@example.test",
        Role::Owner,
        founder,
        "invite-co-owner-01",
    )
    .await;

    let co_owner = store
        .accept_invitation(
            AcceptInvitation {
                token: co_owner_invitation.material.token().to_owned(),
                display_name: "Co-Owner".to_owned(),
                password_hash: hash("correct horse battery staple").unwrap(),
            },
            &SessionMaterial::generate(),
            Duration::hours(1),
        )
        .await
        .unwrap()
        .user;

    let minted_invitation = mint_invitation(
        &store,
        &master_key,
        2,
        "grantee@example.test",
        Role::Viewer,
        co_owner.id,
        "invite-grantee-01",
    )
    .await;

    store
        .update_user_role(co_owner.id, Role::Viewer, co_owner.etag, founder)
        .await
        .unwrap();

    let (revoked_at, revoked_by) = revocation(&store, minted_invitation.invitation.id).await;
    assert!(revoked_at.is_some());
    assert_eq!(revoked_by, Some(founder));

    assert_eq!(
        audit_actor(&store, "invitation.revoke_for_role_change", co_owner.id).await,
        Some(founder)
    );
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn oidc_role_sync_demotion_retires_minted_invitations_with_null_actor_and_audit_attribution()
{
    let db = olp_db::test_support::TestDb::create_migrated("invitation-retirement").await;
    let store = db.store(5).await;
    let master_key = MasterKey::new(1, [17; 32]);
    let founder = owner_id(&store, "oidc-sync").await;

    let configuration_id = Uuid::now_v7();
    let config = store
        .upsert_oidc_configuration(UpsertOidcConfiguration {
            id: configuration_id,
            discovery_url: "https://idp.example.test/.well-known/openid-configuration".to_owned(),
            issuer: "https://idp.example.test".to_owned(),
            authorization_endpoint: "https://idp.example.test/authorize".to_owned(),
            token_endpoint: "https://idp.example.test/token".to_owned(),
            jwks_uri: "https://idp.example.test/jwks".to_owned(),
            token_endpoint_auth_method: "client_secret_basic".to_owned(),
            client_id: "olp".to_owned(),
            encrypted_client_secret: master_key
                .seal(b"client-secret", &oidc_client_secret(configuration_id))
                .unwrap(),
            scopes: vec!["openid".to_owned(), "email".to_owned()],
            email_claim: "email".to_owned(),
            groups_claim: "groups".to_owned(),
            default_role: None,
            email_role_mappings: Vec::new(),
            group_role_mappings: Vec::new(),
            enabled: true,
            actor_user_id: founder,
            expected_etag: None,
        })
        .await
        .unwrap();

    let oidc_session_1 = SessionMaterial::generate();
    let oidc_user = store
        .complete_oidc_login(CompleteOidcLogin {
            configuration_id,
            configuration_etag: config.etag,
            issuer: "https://idp.example.test",
            subject: "oidc-subject-01",
            email: Some("oidc-owner@example.test"),
            display_name: Some("OIDC Owner"),
            provisioning_role: Some(Role::Owner),
            session: &oidc_session_1,
            session_ttl: Duration::hours(1),
        })
        .await
        .unwrap();
    assert_eq!(oidc_user.role, Role::Owner);

    let minted_invitation = mint_invitation(
        &store,
        &master_key,
        3,
        "oidc-grantee@example.test",
        Role::Viewer,
        oidc_user.id,
        "invite-oidc-grantee-01",
    )
    .await;

    let oidc_session_2 = SessionMaterial::generate();
    let demoted_user = store
        .complete_oidc_login(CompleteOidcLogin {
            configuration_id,
            configuration_etag: config.etag,
            issuer: "https://idp.example.test",
            subject: "oidc-subject-01",
            email: Some("oidc-owner@example.test"),
            display_name: Some("OIDC Owner"),
            provisioning_role: Some(Role::Viewer),
            session: &oidc_session_2,
            session_ttl: Duration::hours(1),
        })
        .await
        .unwrap();
    assert_eq!(demoted_user.role, Role::Viewer);

    let (revoked_at, revoked_by) = revocation(&store, minted_invitation.invitation.id).await;
    assert!(revoked_at.is_some());
    assert_eq!(revoked_by, None);

    assert_eq!(
        audit_actor(&store, "user.role_sync_oidc", oidc_user.id).await,
        None
    );
    assert_eq!(
        audit_actor(
            &store,
            "invitation.revoke_for_oidc_role_change",
            oidc_user.id
        )
        .await,
        None
    );
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn manual_invitation_revocation_attributes_acting_operator_on_invitation_and_audit_event() {
    let db = olp_db::test_support::TestDb::create_migrated("invitation-retirement").await;
    let store = db.store(5).await;
    let master_key = MasterKey::new(1, [17; 32]);
    let founder = owner_id(&store, "manual-revoke").await;

    let invitation_result = mint_invitation(
        &store,
        &master_key,
        4,
        "manual-revoke-target@example.test",
        Role::Viewer,
        founder,
        "invite-manual-01",
    )
    .await;

    let revoked = store
        .revoke_invitation(
            invitation_result.invitation.id,
            founder,
            "manual-revoke-key-01",
        )
        .await
        .unwrap();
    assert!(revoked.revoked_at.is_some());
    assert_eq!(
        revoked.revoked_by_email.as_deref(),
        Some("owner@manual-revoke.test")
    );

    let (revoked_at, revoked_by) = revocation(&store, invitation_result.invitation.id).await;
    assert!(revoked_at.is_some());
    assert_eq!(revoked_by, Some(founder));

    assert_eq!(
        audit_actor(&store, "invitation.revoke", invitation_result.invitation.id).await,
        Some(founder)
    );
}
